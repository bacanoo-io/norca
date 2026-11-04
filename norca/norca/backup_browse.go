// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/helper/consulkv"
	"github.com/bacanoo-io/norca/norca/helper/snapshot"
	"github.com/bacanoo-io/norca/norca/norca/backup"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
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

// aclObject reports whether the caller may list an ACL policy or role.
// clean room bugfix: like ACL.ListPolicies and ACL.ListRoles, only a
// management token lists them all; backup read alone revealed every name
// and description.
func (f *backupContentsFilter) aclObject(name string) bool {
	return f.name(name) && (f.aclObj == nil || f.aclObj.IsManagement())
}

// variableItems reports whether the caller may see the item names of a
// Variable. clean room bugfix: the names need read, list shows the paths
// only, as in the Variables API.
func (f *backupContentsFilter) variableItems(ns, path string) bool {
	return f.aclObj == nil || f.aclObj.AllowVariableOperation(ns, path, acl.VariablesCapabilityRead, nil)
}

// consulKV reports whether the caller may list a key of the Consul KV: the
// Variables rules of its path below consul/kv in the namespace of the
// request ("default" for all namespaces).
// consulKV filters the keys of the Consul KV by namespace, the prefix of
// the key and the Variables rules of the namespace and path of the key.
func (f *backupContentsFilter) consulKV(e *consulkv.Entry) bool {
	return backupNamespaceMatches(f.namespace, e.Namespace) &&
		strings.HasPrefix(e.Key, f.prefix) &&
		(f.aclObj == nil || f.aclObj.AllowVariableOperation(e.Namespace,
			consulkv.ACLPath(e.Path, e.Key), acl.VariablesCapabilityList, nil))
}

// consulKVItemID is the ID of a key of the Consul KV in diffs and restore
// items: "<path>:<key>", only the key when the path is consul/kv.
func consulKVItemID(path, key string) string {
	if path == consulkv.DefaultPath {
		return key
	}
	return path + ":" + key
}

// splitConsulKVItemID returns the path and the key of a consulKVItemID; a
// Variables path holds no ":".
func splitConsulKVItemID(id string) (path, key string) {
	if path, key, ok := strings.Cut(id, ":"); ok {
		return path, key
	}
	return consulkv.DefaultPath, id
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
		Namespace: e.Namespace, Path: e.Path, Key: e.Key,
		Size: len(e.Value), Revision: e.Revision, Created: e.Created,
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
			if keyring != nil && f.variableItems(v.Namespace, v.Path) {
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
			if f.aclObject(p.Name) {
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
			if f.aclObject(r.Name) {
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
				if f.consulKV(e) {
					out.ConsulKV = append(out.ConsulKV, consulKVStub(e))
				}
			}
		}

	default:
		objects, ok, err := backupObjects(store, contentType, f)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, structs.NewErrRPCCodedf(400, "unknown content type %q, must be one of %s",
				contentType, strings.Join(structs.BackupContentTypes, ", "))
		}
		out.Objects = objects
	}
	return out, nil
}

// allowed reports whether the caller passes an ACL check; everything passes
// without an ACL object (no ACLs, the preview of a full restore).
func (f *backupContentsFilter) allowed(check func(*acl.ACL) bool) bool {
	return f.aclObj == nil || check(f.aclObj)
}

// management reports whether the caller holds a management token.
func (f *backupContentsFilter) management() bool {
	return f.allowed((*acl.ACL).IsManagement)
}

// backupObjects returns the objects of the content types that share the
// BackupObjectStub form, for browsing and for comparisons; ok is false for
// any other type. The read rules of each type are those of its List
// endpoint.
func backupObjects(store *state.StateStore, contentType string, f *backupContentsFilter) (out []*structs.BackupObjectStub, ok bool, err error) {
	ws := memdb.NewWatchSet()
	add := func(ns, id, detail string, modifyIndex uint64) {
		out = append(out, &structs.BackupObjectStub{Namespace: ns, ID: id, Detail: detail, ModifyIndex: modifyIndex})
	}
	// each runs visit on every row of an iterator.
	each := func(iter memdb.ResultIterator, err error, visit func(raw any)) error {
		if err != nil {
			return err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			visit(raw)
		}
		return nil
	}
	namespaced := func(ns, id string) bool {
		return backupNamespaceMatches(f.namespace, ns) && strings.HasPrefix(id, f.prefix)
	}

	ok = true
	switch contentType {
	case structs.BackupContentQuotas:
		if f.allowed((*acl.ACL).AllowQuotaRead) {
			iter, iterErr := store.QuotaSpecs(ws, state.SortDefault)
			err = each(iter, iterErr, func(raw any) {
				if q := raw.(*structs.QuotaSpec); f.name(q.Name) {
					add("", q.Name, q.Description, q.ModifyIndex)
				}
			})
		}

	case structs.BackupContentAdmissionPolicies:
		if f.allowed((*acl.ACL).AllowAdmissionRead) {
			iter, iterErr := store.AdmissionPolicies(ws, state.SortDefault)
			err = each(iter, iterErr, func(raw any) {
				if p := raw.(*structs.AdmissionPolicy); f.name(p.Name) {
					add("", p.Name, p.Description, p.ModifyIndex)
				}
			})
		}

	case structs.BackupContentNetworkPolicies:
		iter, iterErr := store.NetworkPolicies(ws, state.SortDefault)
		err = each(iter, iterErr, func(raw any) {
			p := raw.(*structs.NetworkPolicy)
			if namespaced(p.Namespace, p.Name) && f.allowed(func(a *acl.ACL) bool { return a.AllowNetworkPolicyRead(p.Namespace) }) {
				add(p.Namespace, p.Name, p.Description, p.ModifyIndex)
			}
		})

	case structs.BackupContentAdminNetworkPolicies:
		if f.allowed((*acl.ACL).AllowOperatorRead) {
			iter, iterErr := store.AdminNetworkPolicies(ws, state.SortDefault)
			err = each(iter, iterErr, func(raw any) {
				if p := raw.(*structs.AdminNetworkPolicy); f.name(p.Name) {
					add("", p.Name, p.Description, p.ModifyIndex)
				}
			})
		}

	case structs.BackupContentServiceIntentions:
		var intentions []*structs.ServiceIntention
		intentions, err = store.ServiceIntentions(ws, structs.AllNamespacesSentinel)
		for _, si := range intentions {
			id := si.SourceNamespace + "/" + si.Source + " -> " + si.Destination
			if namespaced(si.Namespace, id) && f.allowed(func(a *acl.ACL) bool {
				return a.AllowNsOp(si.Namespace, acl.NamespaceCapabilityReadJob)
			}) {
				add(si.Namespace, id, si.Action, si.ModifyIndex)
			}
		}

	case structs.BackupContentExternalServices:
		iter, iterErr := store.GetServiceRegistrations(ws)
		err = each(iter, iterErr, func(raw any) {
			reg := raw.(*structs.ServiceRegistration)
			if reg.IsExternal() && namespaced(reg.Namespace, reg.ID) && f.allowed(func(a *acl.ACL) bool {
				return a.AllowNsOp(reg.Namespace, acl.NamespaceCapabilityReadJob)
			}) {
				add(reg.Namespace, reg.ID, externalServiceDetail(reg), reg.ModifyIndex)
			}
		})

	case structs.BackupContentObjectKinds:
		iter, iterErr := store.ObjectKinds(ws, "", state.SortDefault)
		err = each(iter, iterErr, func(raw any) {
			k := raw.(*structs.ObjectKind)
			if f.name(k.KindRef()) && f.allowed(func(a *acl.ACL) bool { return a.AllowObjectKindRead(k.KindRef()) }) {
				add("", k.KindRef(), k.Description, k.ModifyIndex)
			}
		})

	case structs.BackupContentCustomObjects:
		var kinds []*structs.ObjectKind
		iter, iterErr := store.ObjectKinds(ws, "", state.SortDefault)
		if err = each(iter, iterErr, func(raw any) { kinds = append(kinds, raw.(*structs.ObjectKind)) }); err != nil {
			return nil, true, err
		}
		for _, k := range kinds {
			iter, iterErr := store.CustomObjects(ws, k.Group, k.Kind, structs.AllNamespacesSentinel, state.SortDefault)
			if err = each(iter, iterErr, func(raw any) {
				o := raw.(*structs.CustomObject)
				id := k.KindRef() + "/" + o.Name
				if namespaced(o.Namespace, id) && f.allowed(func(a *acl.ACL) bool {
					return customObjectReadable(a, k, o)
				}) {
					add(o.Namespace, id, "", o.ModifyIndex)
				}
			}); err != nil {
				return nil, true, err
			}
		}

	case structs.BackupContentSecretEngines:
		iter, iterErr := store.SecretEngines(ws)
		err = each(iter, iterErr, func(raw any) {
			e := raw.(*structs.SecretEngine)
			if namespaced(e.Namespace, e.Name) && f.allowed(func(a *acl.ACL) bool {
				return a.AllowNsOp(e.Namespace, acl.NamespaceCapabilityReadSecretEngine)
			}) {
				add(e.Namespace, e.Name, e.Type, e.ModifyIndex)
			}
		})

	case structs.BackupContentSecretLeases:
		iter, iterErr := store.SecretLeases(ws)
		err = each(iter, iterErr, func(raw any) {
			l := raw.(*structs.SecretLease)
			if namespaced(l.Namespace, l.ID) && f.allowed(func(a *acl.ACL) bool {
				return a.AllowNsOp(l.Namespace, acl.NamespaceCapabilityReadSecretEngine)
			}) {
				add(l.Namespace, l.ID, l.Engine+"/"+l.Role, l.ModifyIndex)
			}
		})

	case structs.BackupContentACLTokens:
		if f.management() {
			iter, iterErr := store.ACLTokens(ws, state.SortDefault)
			err = each(iter, iterErr, func(raw any) {
				if t := raw.(*structs.ACLToken); f.name(t.AccessorID) {
					add("", t.AccessorID, t.Name, t.ModifyIndex)
				}
			})
		}

	case structs.BackupContentACLAuthMethods:
		if f.management() {
			iter, iterErr := store.GetACLAuthMethods(ws)
			err = each(iter, iterErr, func(raw any) {
				if m := raw.(*structs.ACLAuthMethod); f.name(m.Name) {
					add("", m.Name, m.Type, m.ModifyIndex)
				}
			})
		}

	case structs.BackupContentACLBindingRules:
		if f.management() {
			iter, iterErr := store.GetACLBindingRules(ws)
			err = each(iter, iterErr, func(raw any) {
				if r := raw.(*structs.ACLBindingRule); f.name(r.ID) {
					add("", r.ID, r.Description, r.ModifyIndex)
				}
			})
		}

	case structs.BackupContentNodePools:
		iter, iterErr := store.NodePools(ws, state.SortDefault)
		err = each(iter, iterErr, func(raw any) {
			p := raw.(*structs.NodePool)
			if f.name(p.Name) && f.allowed(func(a *acl.ACL) bool { return a.AllowNodePool(p.Name) }) {
				add("", p.Name, p.Description, p.ModifyIndex)
			}
		})

	default:
		ok = false
	}
	if err != nil {
		return nil, true, err
	}
	return out, ok, nil
}

// externalServiceDetail describes an external service in the lists of a
// backup: where it points to, and where it comes from.
func externalServiceDetail(reg *structs.ServiceRegistration) string {
	target := reg.Address
	if reg.External.Kind == structs.ExternalServiceKindAlias {
		target = "alias " + target
	} else if reg.Port > 0 {
		target = net.JoinHostPort(reg.Address, strconv.Itoa(reg.Port))
	}
	return target + " (" + reg.External.Source + ")"
}

// customObjectReadable applies the read rules of CustomObject.List: release
// revisions also need read-job in their namespace.
func customObjectReadable(aclObj *acl.ACL, kind *structs.ObjectKind, o *structs.CustomObject) bool {
	ref := kind.KindRef()
	if !aclObj.AllowObjectOperation(objectACLNamespace(kind, o.Namespace), ref, acl.ObjectCapabilityRead) {
		return false
	}
	return ref != structs.ReleaseRevisionKindRef || aclObj.AllowNsOp(o.Namespace, acl.NamespaceCapabilityReadJob)
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
				if f.consulKV(e) {
					add(&backupDiffEntry{namespace: e.Namespace, id: consulKVItemID(e.Path, e.Key),
						status: "sha256:" + consulKVStub(e).SHA256[:12]})
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
			if f.aclObject(p.Name) {
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
			if f.aclObject(r.Name) {
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
		objects, ok, err := backupObjects(store, contentType, f)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, structs.NewErrRPCCodedf(400, "unknown content type %q", contentType)
		}
		for _, o := range objects {
			add(&backupDiffEntry{namespace: o.Namespace, id: o.ID, modifyIndex: o.ModifyIndex})
		}
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
