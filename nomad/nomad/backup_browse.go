// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/helper/snapshot"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// backupRestoreFunc returns the function that restores a backup's snapshot
// archive into a separate state store. It mirrors
// helper/raftutil.RestoreFromArchive, which imports this package and can't
// be used here: a standalone FSM restores the archive, nothing touches the
// running cluster.
func backupRestoreFunc(logger hclog.Logger, region string) backup.RestoreFunc {
	return func(archive io.Reader) (*state.StateStore, error) {
		evalBroker, err := NewEvalBroker(1, 1, 1, 1)
		if err != nil {
			return nil, err
		}
		fsm, err := NewFSM(&FSMConfig{
			EvalBroker: evalBroker,
			Periodic:   NewPeriodicDispatch(logger, nil),
			Blocked:    NewBlockedEvals(nil, logger),
			Logger:     logger,
			Region:     region,
			// Only used by upserts; a restore keeps every version found in
			// the backup.
			JobTrackedVersions: structs.JobDefaultTrackedVersions,
		})
		if err != nil {
			return nil, err
		}

		r, w := io.Pipe()
		errCh := make(chan error, 1)
		go func() {
			_, err := snapshot.CopySnapshot(archive, w)
			if err != nil {
				w.CloseWithError(err)
			}
			errCh <- err
		}()

		restoreErr := fsm.RestoreWithFilter(r, nil)
		r.CloseWithError(fmt.Errorf("restore finished"))
		copyErr := <-errCh
		if copyErr != nil {
			return nil, fmt.Errorf("failed to read the snapshot archive: %w", copyErr)
		}
		if restoreErr != nil {
			return nil, fmt.Errorf("failed to restore the snapshot: %w", restoreErr)
		}
		return fsm.State(), nil
	}
}

// backupNamespaceMatches reports whether a namespace matches the request's
// namespace; "*" matches all.
func backupNamespaceMatches(want, ns string) bool {
	return want == "" || want == structs.AllNamespacesSentinel || want == ns
}

// backupContentsFilter selects the objects of a browse or diff request the
// caller may see.
type backupContentsFilter struct {
	aclObj    *acl.ACL
	namespace string
	jobID     string
	prefix    string
}

func (f *backupContentsFilter) job(ns, id string) bool {
	return backupNamespaceMatches(f.namespace, ns) &&
		(f.jobID == "" || f.jobID == id) &&
		strings.HasPrefix(id, f.prefix) &&
		f.aclObj.AllowNsOp(ns, acl.NamespaceCapabilityReadJob)
}

func (f *backupContentsFilter) variable(ns, path string) bool {
	return backupNamespaceMatches(f.namespace, ns) &&
		strings.HasPrefix(path, f.prefix) &&
		(f.aclObj == nil || f.aclObj.AllowVariableOperation(ns, path, acl.VariablesCapabilityList, nil))
}

func (f *backupContentsFilter) secret(ns, path string) bool {
	return backupNamespaceMatches(f.namespace, ns) &&
		strings.HasPrefix(path, f.prefix) &&
		f.aclObj.AllowSecretOperation(ns, path, acl.NamespaceCapabilityListSecrets)
}

func (f *backupContentsFilter) name(name string) bool {
	return strings.HasPrefix(name, f.prefix)
}

// consulKV reports whether the caller may list a key of the Consul KV: the
// Variables rules of its path below consul/kv in the namespace of the
// request ("default" for all namespaces).
func (f *backupContentsFilter) consulKV(key string) bool {
	return strings.HasPrefix(key, f.prefix) &&
		(f.aclObj == nil || f.aclObj.AllowVariableOperation(consulKVNamespace(f.namespace),
			consulkv.ACLPath(consulkv.DefaultPath, key), acl.VariablesCapabilityList, nil))
}

// consulKVNamespace is the namespace whose Variables rules apply to the keys
// of the Consul KV in backups: the one of the request, "default" without
// one.
func consulKVNamespace(ns string) string {
	if ns == "" || ns == "*" {
		return structs.DefaultNamespace
	}
	return ns
}

// consulKVStub summarizes a key of the Consul KV without its value.
func consulKVStub(e *consulkv.Entry) *structs.BackupConsulKVStub {
	sum := sha256.Sum256(e.Value)
	return &structs.BackupConsulKVStub{
		Key: e.Key, Size: len(e.Value), Revision: e.Revision, Created: e.Created,
		SHA256: hex.EncodeToString(sum[:]),
	}
}

// backupSide is one side of a diff: a state store and the Consul KV.
type backupSide struct {
	state    *state.StateStore
	consulKV *consulkv.Export
}

// backupContents returns the objects of one content type of a backup.
// Variables values are never returned; their key names are when the
// backup's keyring could be read.
func backupContents(loaded *backup.Loaded, contentType string, f *backupContentsFilter) (*structs.BackupContents, error) {
	store := loaded.State
	ws := memdb.NewWatchSet()
	out := &structs.BackupContents{
		BackupID:  loaded.Record.ID,
		RaftIndex: loaded.Record.RaftIndex,
		Type:      contentType,
	}

	switch contentType {
	case structs.BackupContentNamespaces:
		iter, err := store.Namespaces(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			ns := raw.(*structs.Namespace)
			if f.name(ns.Name) && f.aclObj.AllowNamespace(ns.Name) {
				out.Namespaces = append(out.Namespaces, ns)
			}
		}

	case structs.BackupContentJobs:
		iter, err := store.Jobs(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			job := raw.(*structs.Job)
			if !f.job(job.Namespace, job.ID) {
				continue
			}
			stub := &structs.BackupJobStub{
				ID: job.ID, Namespace: job.Namespace, Name: job.Name, Type: job.Type,
				Status: job.Status, Stop: job.Stop, Version: job.Version, SubmitTime: job.SubmitTime,
				JobModifyIndex: job.JobModifyIndex, ModifyIndex: job.ModifyIndex,
			}
			versions, err := store.JobVersionsByID(ws, job.Namespace, job.ID)
			if err != nil {
				return nil, err
			}
			for _, v := range versions {
				stub.Versions = append(stub.Versions, v.Version)
			}
			out.Jobs = append(out.Jobs, stub)
		}

	case structs.BackupContentJobVersions:
		if f.jobID == "" {
			return nil, structs.NewErrRPCCodedf(400, "the job-versions content type requires a job ID")
		}
		iter, err := store.JobVersions(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			job := raw.(*structs.Job)
			if f.job(job.Namespace, job.ID) {
				out.JobVersions = append(out.JobVersions, job)
			}
		}
		sort.SliceStable(out.JobVersions, func(i, j int) bool {
			return out.JobVersions[i].Version > out.JobVersions[j].Version
		})

	case structs.BackupContentVariables:
		var keyring *Encrypter
		if loaded.Keys != nil {
			var err error
			if keyring, err = newBackupKeyring(loaded.Keys); err != nil {
				return nil, err
			}
		}
		iter, err := store.Variables(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			v := raw.(*structs.VariableEncrypted)
			if !f.variable(v.Namespace, v.Path) {
				continue
			}
			stub := &structs.BackupVariableStub{
				Namespace: v.Namespace, Path: v.Path, CreateTime: v.CreateTime,
				ModifyTime: v.ModifyTime, ModifyIndex: v.ModifyIndex,
			}
			if keyring != nil {
				if items, err := decryptBackupVariable(keyring, v); err == nil {
					for k := range items {
						stub.Keys = append(stub.Keys, k)
					}
					slices.Sort(stub.Keys)
				}
			}
			out.Variables = append(out.Variables, stub)
		}

	case structs.BackupContentSecrets:
		// Only the metadata rows: the values are in the barrier's
		// ciphertext blobs, which are never decrypted for browsing.
		iter, err := store.Secrets(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			secret := raw.(*structs.Secret)
			if f.secret(secret.Namespace, secret.Path) {
				out.Secrets = append(out.Secrets, secret.Stub())
			}
		}

	case structs.BackupContentACLPolicies:
		iter, err := store.ACLPolicies(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			p := raw.(*structs.ACLPolicy)
			if f.name(p.Name) {
				out.ACLPolicies = append(out.ACLPolicies, p.Stub())
			}
		}

	case structs.BackupContentACLRoles:
		iter, err := store.GetACLRoles(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			r := raw.(*structs.ACLRole)
			if f.name(r.Name) {
				out.ACLRoles = append(out.ACLRoles, r.Stub())
			}
		}

	case structs.BackupContentDeployments:
		iter, err := store.Deployments(ws, state.SortDefault)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			d := raw.(*structs.Deployment)
			if f.job(d.Namespace, d.JobID) {
				out.Deployments = append(out.Deployments, d)
			}
		}

	case structs.BackupContentAllocations:
		iter, err := store.Allocs(ws, state.SortDefault)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			a := raw.(*structs.Allocation)
			if f.job(a.Namespace, a.JobID) {
				out.Allocations = append(out.Allocations, a.Stub(nil))
			}
		}

	case structs.BackupContentConsulKV:
		if loaded.ConsulKV != nil {
			for _, e := range loaded.ConsulKV.Entries {
				if f.consulKV(e.Key) {
					out.ConsulKV = append(out.ConsulKV, consulKVStub(e))
				}
			}
		}

	default:
		return nil, structs.NewErrRPCCodedf(400, "unknown content type %q, must be one of %s",
			contentType, strings.Join(structs.BackupContentTypes, ", "))
	}
	return out, nil
}

// decryptBackupVariable decrypts a Variables entry with a keyring.
func decryptBackupVariable(keyring *Encrypter, v *structs.VariableEncrypted) (structs.VariableItems, error) {
	b, err := keyring.Decrypt(v.Data, v.KeyID)
	if err != nil {
		return nil, err
	}
	items := make(structs.VariableItems)
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// backupDiffEntry is an object compared between two sides of a diff.
type backupDiffEntry struct {
	namespace   string
	id          string
	version     *uint64
	modifyIndex uint64
	status      string
}

// backupDiffEntries collects the comparable objects of a content type.
func backupDiffEntries(side backupSide, contentType string, f *backupContentsFilter) (map[string]*backupDiffEntry, error) {
	store := side.state
	ws := memdb.NewWatchSet()
	out := make(map[string]*backupDiffEntry)
	add := func(e *backupDiffEntry) {
		out[e.namespace+"\x00"+e.id] = e
	}

	switch contentType {
	case structs.BackupContentConsulKV:
		// Keys are compared by the hash of their value: the revisions of
		// a bucket are not comparable across backups.
		if side.consulKV != nil {
			for _, e := range side.consulKV.Entries {
				if f.consulKV(e.Key) {
					add(&backupDiffEntry{id: e.Key, status: "sha256:" + consulKVStub(e).SHA256[:12]})
				}
			}
		}
	case structs.BackupContentNamespaces:
		iter, err := store.Namespaces(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			ns := raw.(*structs.Namespace)
			if f.name(ns.Name) && f.aclObj.AllowNamespace(ns.Name) {
				add(&backupDiffEntry{id: ns.Name, modifyIndex: ns.ModifyIndex})
			}
		}
	case structs.BackupContentJobs:
		iter, err := store.Jobs(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			job := raw.(*structs.Job)
			if f.job(job.Namespace, job.ID) {
				v := job.Version
				// JobModifyIndex changes only when the job spec changes.
				add(&backupDiffEntry{namespace: job.Namespace, id: job.ID, version: &v,
					modifyIndex: job.JobModifyIndex, status: job.Status})
			}
		}
	case structs.BackupContentJobVersions:
		iter, err := store.JobVersions(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			job := raw.(*structs.Job)
			if f.job(job.Namespace, job.ID) {
				v := job.Version
				add(&backupDiffEntry{namespace: job.Namespace, id: fmt.Sprintf("%s@%d", job.ID, job.Version),
					version: &v, modifyIndex: job.JobModifyIndex})
			}
		}
	case structs.BackupContentVariables:
		iter, err := store.Variables(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			v := raw.(*structs.VariableEncrypted)
			if f.variable(v.Namespace, v.Path) {
				add(&backupDiffEntry{namespace: v.Namespace, id: v.Path, modifyIndex: v.ModifyIndex})
			}
		}
	case structs.BackupContentSecrets:
		iter, err := store.Secrets(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			secret := raw.(*structs.Secret)
			if f.secret(secret.Namespace, secret.Path) {
				v := secret.CurrentVersion
				add(&backupDiffEntry{namespace: secret.Namespace, id: secret.Path, version: &v,
					modifyIndex: secret.ModifyIndex})
			}
		}
	case structs.BackupContentACLPolicies:
		iter, err := store.ACLPolicies(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			p := raw.(*structs.ACLPolicy)
			if f.name(p.Name) {
				add(&backupDiffEntry{id: p.Name, modifyIndex: p.ModifyIndex})
			}
		}
	case structs.BackupContentACLRoles:
		iter, err := store.GetACLRoles(ws)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			r := raw.(*structs.ACLRole)
			if f.name(r.Name) {
				add(&backupDiffEntry{id: r.Name, modifyIndex: r.ModifyIndex})
			}
		}
	case structs.BackupContentDeployments:
		iter, err := store.Deployments(ws, state.SortDefault)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			d := raw.(*structs.Deployment)
			if f.job(d.Namespace, d.JobID) {
				v := d.JobVersion
				add(&backupDiffEntry{namespace: d.Namespace, id: d.ID, version: &v,
					modifyIndex: d.ModifyIndex, status: d.Status})
			}
		}
	case structs.BackupContentAllocations:
		iter, err := store.Allocs(ws, state.SortDefault)
		if err != nil {
			return nil, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			a := raw.(*structs.Allocation)
			if f.job(a.Namespace, a.JobID) {
				add(&backupDiffEntry{namespace: a.Namespace, id: a.ID,
					modifyIndex: a.AllocModifyIndex, status: a.ClientStatus + "/" + a.DesiredStatus})
			}
		}
	default:
		return nil, structs.NewErrRPCCodedf(400, "unknown content type %q", contentType)
	}
	return out, nil
}

// backupDiff compares two state stores: from is a backup, to the current
// state or a newer backup. Without a content type all types are compared.
func backupDiff(from, to backupSide, contentType string, f *backupContentsFilter) ([]*structs.BackupObjectDiff, error) {
	types := structs.BackupContentTypes
	if contentType != "" {
		types = []string{contentType}
	}

	var out []*structs.BackupObjectDiff
	for _, typ := range types {
		before, err := backupDiffEntries(from, typ, f)
		if err != nil {
			return nil, err
		}
		after, err := backupDiffEntries(to, typ, f)
		if err != nil {
			return nil, err
		}

		var diffs []*structs.BackupObjectDiff
		for key, b := range before {
			a, ok := after[key]
			switch {
			case !ok:
				diffs = append(diffs, &structs.BackupObjectDiff{Type: typ, Change: structs.BackupDiffDeleted,
					Namespace: b.namespace, ID: b.id, FromVersion: b.version, FromModifyIndex: b.modifyIndex,
					FromStatus: b.status})
			case a.modifyIndex != b.modifyIndex || a.status != b.status:
				diffs = append(diffs, &structs.BackupObjectDiff{Type: typ, Change: structs.BackupDiffModified,
					Namespace: b.namespace, ID: b.id,
					FromVersion: b.version, ToVersion: a.version,
					FromModifyIndex: b.modifyIndex, ToModifyIndex: a.modifyIndex,
					FromStatus: b.status, ToStatus: a.status})
			}
		}
		for key, a := range after {
			if _, ok := before[key]; !ok {
				diffs = append(diffs, &structs.BackupObjectDiff{Type: typ, Change: structs.BackupDiffAdded,
					Namespace: a.namespace, ID: a.id, ToVersion: a.version, ToModifyIndex: a.modifyIndex,
					ToStatus: a.status})
			}
		}
		sort.Slice(diffs, func(i, j int) bool {
			if diffs[i].Namespace != diffs[j].Namespace {
				return diffs[i].Namespace < diffs[j].Namespace
			}
			return diffs[i].ID < diffs[j].ID
		})
		out = append(out, diffs...)
	}
	return out, nil
}
