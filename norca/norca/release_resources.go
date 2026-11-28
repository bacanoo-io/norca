// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The resources of a release are applied through the normal RPC endpoints
// of their types, with the token of the caller, so that ACL, admission,
// quotas and validation apply exactly as for direct writes. Their live state
// is read from the state store to plan, to detect drift and to restore the
// previous state of a failed atomic operation.

// liveResource is the live state of a resource of a release.
type liveResource struct {
	ref *structs.ReleaseResourceRef

	// res is the live content, nil if the resource doesn't exist.
	res *structs.ReleaseResource

	modifyIndex uint64
	version     uint64

	// createIndex is the index at which the resource was created: one
	// deleted and created again since the release recorded it has a newer
	// one (review finding ATM-7).
	createIndex uint64

	// hash is the hash of the content (releaseResourceHash), compared in
	// memory only. refHash is what the status of the release records and
	// drift detection compares: the same, except for a Variables entry,
	// whose items are secret and whose status every reader of the release
	// gets (releaseVariableHash).
	hash    string
	refHash string
}

// releaseVariableHashPrefix starts the hash of a Variables entry in the
// status of a release: "hmac-sha256:<root key ID>:<hex>".
const releaseVariableHashPrefix = "hmac-sha256:"

func (l *liveResource) exists() bool { return l != nil && l.res != nil }

// splitObjectRefName splits the name of an object reference
// ("group/Kind/name").
func splitObjectRefName(name string) (group, kind, objName string, err error) {
	parts := strings.SplitN(name, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("invalid object reference %q", name)
	}
	return parts[0], parts[1], parts[2], nil
}

// readLive reads the live state of a resource.
func (m *releaseManager) readLive(snap *state.StateSnapshot, ref *structs.ReleaseResourceRef) (*liveResource, error) {
	live := &liveResource{ref: ref}
	res := &structs.ReleaseResource{Type: ref.Type}
	switch ref.Type {
	case structs.ReleaseResourceNamespace:
		ns, err := snap.NamespaceByName(nil, ref.Name)
		if err != nil || ns == nil {
			return live, err
		}
		res.Namespace = ns.Copy()
		live.modifyIndex, live.createIndex = ns.ModifyIndex, ns.CreateIndex
	case structs.ReleaseResourceQuota:
		q, err := snap.QuotaSpecByName(nil, ref.Name)
		if err != nil || q == nil {
			return live, err
		}
		res.Quota = q.Copy()
		live.modifyIndex, live.createIndex = q.ModifyIndex, q.CreateIndex
	case structs.ReleaseResourceACLPolicy:
		p, err := snap.ACLPolicyByName(nil, ref.Name)
		if err != nil || p == nil {
			return live, err
		}
		cp := *p
		res.ACLPolicy = &cp
		live.modifyIndex, live.createIndex = p.ModifyIndex, p.CreateIndex
	case structs.ReleaseResourceVariable:
		v, err := snap.GetVariable(nil, ref.Namespace, ref.Name)
		if err != nil || v == nil {
			return live, err
		}
		plain, err := m.srv.encrypter.Decrypt(v.Data, v.KeyID)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt variable %q: %w", ref.Name, err)
		}
		dv := &structs.VariableDecrypted{VariableMetadata: v.VariableMetadata, Items: map[string]string{}}
		if err := json.Unmarshal(plain, &dv.Items); err != nil {
			return nil, err
		}
		res.Variable = dv
		live.modifyIndex, live.createIndex = v.ModifyIndex, v.CreateIndex
		if live.refHash, err = m.releaseVariableHash(ref, v.KeyID, dv.Items); err != nil {
			return nil, err
		}
	case structs.ReleaseResourceObjectKind:
		group, kind, err := structs.ParseObjectKindRef(ref.Name)
		if err != nil {
			return nil, err
		}
		k, err := snap.ObjectKindByName(nil, group, kind)
		if err != nil || k == nil {
			return live, err
		}
		res.ObjectKind = k.Copy()
		live.modifyIndex, live.createIndex = k.ModifyIndex, k.CreateIndex
	case structs.ReleaseResourceAdmissionPolicy:
		p, err := snap.AdmissionPolicyByName(nil, ref.Name)
		if err != nil || p == nil {
			return live, err
		}
		res.AdmissionPolicy = p.Copy()
		live.modifyIndex, live.createIndex = p.ModifyIndex, p.CreateIndex
	case structs.ReleaseResourceExternalService:
		reg, err := snap.GetServiceRegistrationByID(nil, ref.Namespace, ref.Name)
		if err != nil || reg == nil {
			return live, err
		}
		if !reg.IsExternal() {
			return nil, fmt.Errorf("service ID %q in namespace %q is used by a registration of an allocation",
				ref.Name, ref.Namespace)
		}
		res.ExternalService = &structs.ReleaseExternalService{
			Namespace: reg.Namespace,
			Service:   structs.ExternalServiceSpecFromRegistration(reg),
		}
		live.modifyIndex, live.createIndex = reg.ModifyIndex, reg.CreateIndex
	case structs.ReleaseResourceObject:
		group, kind, name, err := splitObjectRefName(ref.Name)
		if err != nil {
			return nil, err
		}
		o, err := snap.CustomObjectByName(nil, group, kind, ref.Namespace, name)
		if err != nil || o == nil {
			return live, err
		}
		res.Object = o.Copy()
		live.modifyIndex, live.createIndex = o.ModifyIndex, o.CreateIndex
		live.version = o.Generation
	case structs.ReleaseResourceJob:
		j, err := snap.JobByID(nil, ref.Namespace, ref.Name)
		if err != nil || j == nil {
			return live, err
		}
		res.Job = j.Copy()
		live.modifyIndex, live.createIndex = j.JobModifyIndex, j.CreateIndex
		live.version = j.Version
	default:
		return nil, fmt.Errorf("unknown resource type %q", ref.Type)
	}
	live.res = res
	live.hash = releaseResourceHash(res)
	if live.refHash == "" {
		live.refHash = live.hash
	}
	return live, nil
}

// releaseVariableHash is the hash of the items of a Variables entry that
// the status of a release records for drift detection. The status is read
// with the read right on the release alone, and a plain hash of the items
// could be tried offline against a guessed secret item, so the hash is
// keyed: HMAC-SHA256 with a key derived from a root key of the keyring,
// bound to the namespace and path. The ID of the root key is part of the
// hash ("hmac-sha256:<key ID>:<hex>"): a hash recorded in ref is computed
// again with its key, so that a Variable written again unchanged after a
// key rotation is no drift. A new hash, and one whose key is no longer in
// the keyring (which then compares as modified until the release is
// applied again), uses the key the Variable is encrypted with.
func (m *releaseManager) releaseVariableHash(ref *structs.ReleaseResourceRef, keyID string, items map[string]string) (string, error) {
	var key []byte
	if rest, ok := strings.CutPrefix(ref.Hash, releaseVariableHashPrefix); ok {
		if id, _, ok := strings.Cut(rest, ":"); ok && id != "" {
			if k, err := m.srv.encrypter.GetKey(id); err == nil {
				keyID, key = id, k
			}
		}
	}
	if key == nil {
		k, err := m.srv.encrypter.GetKey(keyID)
		if err != nil {
			return "", fmt.Errorf("failed to hash variable %q: %w", ref.Name, err)
		}
		key = k
	}
	// The root key encrypts the Variables: the hash gets its own key.
	derive := hmac.New(sha256.New, key)
	derive.Write([]byte("norca release variable hash v1"))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	mac.Write([]byte(ref.Namespace))
	mac.Write([]byte{0})
	mac.Write([]byte(ref.Name))
	mac.Write([]byte{0})
	// encoding/json writes the keys of a map sorted.
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	mac.Write(data)
	return releaseVariableHashPrefix + keyID + ":" + hex.EncodeToString(mac.Sum(nil)), nil
}

// releaseResourceHash hashes the user-controlled content of a resource, so
// that a change of the live state can be detected. Indexes, hashes, times
// and status fields are left out.
func releaseResourceHash(res *structs.ReleaseResource) string {
	var v any
	switch res.Type {
	case structs.ReleaseResourceNamespace:
		ns := res.Namespace.Copy()
		ns.CreateIndex, ns.ModifyIndex, ns.Hash = 0, 0, nil
		if len(ns.Meta) == 0 {
			ns.Meta = nil
		}
		v = ns
	case structs.ReleaseResourceQuota:
		q := res.Quota.Copy()
		q.CreateIndex, q.ModifyIndex, q.Hash = 0, 0, nil
		// The servers store the hash of every limit too: without this a
		// stored quota never equals the one of a pack.
		for _, l := range q.Limits {
			if l != nil {
				l.Hash = nil
			}
		}
		v = q
	case structs.ReleaseResourceACLPolicy:
		p := *res.ACLPolicy
		p.CreateIndex, p.ModifyIndex, p.Hash = 0, 0, nil
		// RulesJSON is made from Rules when a policy is read, not written.
		p.RulesJSON = nil
		v = p
	case structs.ReleaseResourceVariable:
		v = res.Variable.Items
	case structs.ReleaseResourceObjectKind:
		// Approvals of webhooks are server state, and the servers fill in
		// the defaults of the ecosystem fields.
		k := res.ObjectKind.WithoutApprovals()
		k.Canonicalize()
		k.CreateIndex, k.ModifyIndex = 0, 0
		v = k
	case structs.ReleaseResourceAdmissionPolicy:
		p := res.AdmissionPolicy.Copy()
		p.CreateIndex, p.ModifyIndex = 0, 0
		v = p
	case structs.ReleaseResourceExternalService:
		spec, err := res.ExternalService.Service.Normalized()
		if err != nil {
			spec = res.ExternalService.Service
		}
		v = map[string]any{"namespace": res.ExternalService.Namespace, "service": spec}
	case structs.ReleaseResourceObject:
		v = map[string]any{
			"spec":      res.Object.Spec,
			"labels":    nonEmptyLabels(res.Object.Labels),
			"managedBy": res.Object.ManagedBy,
		}
	case structs.ReleaseResourceJob:
		j := res.Job.Copy()
		j.Status, j.StatusDescription, j.Stable = "", "", false
		j.Version, j.SubmitTime = 0, 0
		j.CreateIndex, j.ModifyIndex, j.JobModifyIndex = 0, 0, 0
		// The count of a group that an enabled scaling policy scales belongs
		// to the policy, not to the release: a new version of the job keeps
		// it (keepScaledCounts), so a count the autoscaler or a job scale
		// set is no drift.
		for _, tg := range j.TaskGroups {
			if isScaledGroup(tg) {
				tg.Count = 0
			}
		}
		v = j
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// releaseWriteRequest returns the write request of an internal call made on
// behalf of the caller.
func (op *releaseOp) writeRequest(namespace string) structs.WriteRequest {
	return structs.WriteRequest{
		Region:    op.m.srv.Region(),
		Namespace: namespace,
		AuthToken: op.token,
	}
}

// applyResult is what applying a resource returned.
type applyResult struct {
	evalID   string
	warnings string

	// index is the modify index of the resource after the write (of a job
	// its JobModifyIndex) as the endpoint answered it, 0 when the endpoint
	// does not say (an external service: an unchanged registration keeps
	// its index, whatever the answer says).
	index uint64
}

// errReleaseResourceChanged is the error of a check-and-set write of a
// resource that changed since the release read it: nothing was written.
var errReleaseResourceChanged = errors.New("changed since the release read it")

// changedError is the refusal of a check-and-set write or deletion by the
// endpoint of a type: errReleaseResourceChanged, with the message of the
// endpoint.
type changedError struct{ err error }

func (e *changedError) Error() string   { return e.err.Error() }
func (e *changedError) Unwrap() []error { return []error{errReleaseResourceChanged, e.err} }

// releaseWritesChecked reports whether a resource of the type can be written
// with check-and-set (applyResourceAt).
func releaseWritesChecked(resourceType string) bool {
	switch resourceType {
	case structs.ReleaseResourceNamespace, structs.ReleaseResourceQuota, structs.ReleaseResourceACLPolicy,
		structs.ReleaseResourceVariable, structs.ReleaseResourceObjectKind, structs.ReleaseResourceObject,
		structs.ReleaseResourceJob:
		return true
	}
	return false
}

// releaseDeletesChecked reports whether a resource of the type can be
// deleted with check-and-set (deleteResourceAt).
func releaseDeletesChecked(resourceType string) bool {
	return resourceType == structs.ReleaseResourceVariable || resourceType == structs.ReleaseResourceObject
}

// applyResource creates or updates a resource through the endpoint of its
// type.
func (op *releaseOp) applyResource(res *structs.ReleaseResource) (*applyResult, error) {
	return op.applyResourceAt(res, nil)
}

// applyResourceAt is applyResource, and with index set a check-and-set write
// (releaseWritesChecked): the resource is written only if its ModifyIndex (of
// a job: JobModifyIndex) is still *index, 0 meaning that it must not exist.
// A refused write fails with an error that wraps errReleaseResourceChanged;
// for a job it keeps the message of the job endpoint, which starts with
// RegisterEnforceIndexErrPrefix. Other types can't be written so.
//
// An operation that a leader change interrupted writes nothing
// (release_leader.go): the write fails with errReleaseInterrupted.
func (op *releaseOp) applyResourceAt(res *structs.ReleaseResource, index *uint64) (*applyResult, error) {
	if index != nil && !releaseWritesChecked(res.Type) {
		return nil, fmt.Errorf("a %s can't be written with check-and-set", res.Type)
	}
	if err := op.interruption(); err != nil {
		return nil, err
	}
	out, err := op.applyResourceRPC(res, index)
	if index != nil && err != nil && !errors.Is(err, errReleaseResourceChanged) {
		switch res.Type {
		case structs.ReleaseResourceNamespace, structs.ReleaseResourceQuota, structs.ReleaseResourceACLPolicy:
			if structs.IsIndexConflictError(err) {
				err = &changedError{err}
			}
		case structs.ReleaseResourceObjectKind, structs.ReleaseResourceObject:
			if structs.IsObjectConflictError(err) {
				err = &changedError{err}
			}
		case structs.ReleaseResourceJob:
			if strings.Contains(err.Error(), RegisterEnforceIndexErrPrefix) {
				err = &changedError{err}
			}
		}
	}
	return out, err
}

// applyResourceRPC sends the write of applyResourceAt.
func (op *releaseOp) applyResourceRPC(res *structs.ReleaseResource, index *uint64) (*applyResult, error) {
	srv := op.m.srv
	out := &applyResult{}
	var generic structs.GenericResponse
	switch res.Type {
	case structs.ReleaseResourceNamespace:
		ns := res.Namespace.Copy()
		ns.CreateIndex, ns.ModifyIndex, ns.Hash = 0, 0, nil
		req := &structs.NamespaceUpsertRequest{Namespaces: []*structs.Namespace{ns}, WriteRequest: op.writeRequest("")}
		if index != nil {
			req.ExpectedModifyIndex = map[string]uint64{ns.Name: *index}
		}
		err := srv.RPC("Namespace.UpsertNamespaces", req, &generic)
		out.index = generic.Index
		return out, err
	case structs.ReleaseResourceQuota:
		q := res.Quota.Copy()
		q.CreateIndex, q.ModifyIndex, q.Hash = 0, 0, nil
		req := &structs.QuotaSpecUpsertRequest{Quotas: []*structs.QuotaSpec{q}, WriteRequest: op.writeRequest("")}
		if index != nil {
			req.ExpectedModifyIndex = map[string]uint64{q.Name: *index}
		}
		err := srv.RPC("Quota.UpsertQuotaSpecs", req, &generic)
		out.index = generic.Index
		return out, err
	case structs.ReleaseResourceACLPolicy:
		p := *res.ACLPolicy
		p.CreateIndex, p.ModifyIndex, p.Hash = 0, 0, nil
		req := &structs.ACLPolicyUpsertRequest{Policies: []*structs.ACLPolicy{&p}, WriteRequest: op.writeRequest("")}
		if index != nil {
			req.ExpectedModifyIndex = map[string]uint64{p.Name: *index}
		}
		err := srv.RPC("ACL.UpsertPolicies", req, &generic)
		out.index = generic.Index
		return out, err
	case structs.ReleaseResourceVariable:
		v := res.Variable.Copy()
		v.CreateIndex, v.ModifyIndex, v.CreateTime, v.ModifyTime = 0, 0, 0, 0
		varOp := structs.VarOpSet
		if index != nil {
			varOp, v.ModifyIndex = structs.VarOpCAS, *index
		}
		var reply structs.VariablesApplyResponse
		err := srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
			Op: varOp, Var: &v, WriteRequest: op.writeRequest(v.Namespace),
		}, &reply)
		switch {
		case err != nil:
		case reply.Error != nil:
			err = reply.Error
		case reply.IsConflict():
			err = fmt.Errorf("the variable %q (namespace %q) %w", v.Path, v.Namespace, errReleaseResourceChanged)
		case reply.Output != nil:
			out.index = reply.Output.ModifyIndex
		}
		return out, err
	case structs.ReleaseResourceObjectKind:
		k := res.ObjectKind.Copy()
		k.CreateIndex, k.ModifyIndex = 0, 0
		var reply structs.ObjectKindUpsertResponse
		err := srv.RPC("ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
			ObjectKind: k, CAS: index, WriteRequest: op.writeRequest(""),
		}, &reply)
		if err == nil && reply.ObjectKind != nil {
			out.index = reply.ObjectKind.ModifyIndex
		}
		return out, err
	case structs.ReleaseResourceAdmissionPolicy:
		p := res.AdmissionPolicy.Copy()
		p.CreateIndex, p.ModifyIndex = 0, 0
		err := srv.RPC("AdmissionPolicy.UpsertAdmissionPolicies", &structs.AdmissionPolicyUpsertRequest{
			AdmissionPolicies: []*structs.AdmissionPolicy{p}, WriteRequest: op.writeRequest(""),
		}, &generic)
		out.index = generic.Index
		return out, err
	case structs.ReleaseResourceExternalService:
		reg, err := res.ExternalService.Service.Registration()
		if err != nil {
			return out, err
		}
		ns := res.ExternalService.Namespace
		if ns == "" {
			ns = op.namespace
		}
		var reply structs.ExternalServiceUpsertResponse
		return out, srv.RPC(structs.ServiceRegistrationUpsertExternalRPCMethod, &structs.ExternalServiceUpsertRequest{
			Service: reg, WriteRequest: op.writeRequest(ns),
		}, &reply)
	case structs.ReleaseResourceObject:
		o := res.Object.Copy()
		ns := o.Namespace
		if ns == "" {
			ns = op.namespace
		}
		var reply structs.CustomObjectWriteResponse
		err := srv.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
			Object: o, CAS: index, WriteRequest: op.writeRequest(ns),
		}, &reply)
		if err == nil && reply.Object != nil {
			out.index = reply.Object.ModifyIndex
		}
		return out, err
	case structs.ReleaseResourceJob:
		j := res.Job.Copy()
		j.CreateIndex, j.ModifyIndex, j.JobModifyIndex = 0, 0, 0
		j.Version, j.Stable, j.SubmitTime = 0, false, 0
		j.Status, j.StatusDescription = "", ""
		var sub *structs.JobSubmission
		if res.Submission != nil {
			sub = res.Submission.Copy()
		}
		req := &structs.JobRegisterRequest{Job: j, Submission: sub, WriteRequest: op.writeRequest(j.Namespace)}
		if index != nil {
			req.EnforceIndex, req.JobModifyIndex = true, *index
		}
		var reply structs.JobRegisterResponse
		err := srv.RPC("Job.Register", req, &reply)
		out.evalID, out.warnings = reply.EvalID, reply.Warnings
		if err == nil {
			out.index = reply.JobModifyIndex
		}
		return out, err
	}
	return out, fmt.Errorf("unknown resource type %q", res.Type)
}

// deleteResource deletes a resource through the endpoint of its type. A
// resource that doesn't exist is not an error.
func (op *releaseOp) deleteResource(ref *structs.ReleaseResourceRef) error {
	return op.deleteResourceAt(ref, nil)
}

// deleteResourceAt is deleteResource, and with index set a check-and-set
// deletion (releaseDeletesChecked): the resource is deleted only if its
// ModifyIndex is still *index. A refused deletion fails with an error that
// wraps errReleaseResourceChanged. Other types can't be deleted so. An
// interrupted operation deletes nothing, as applyResourceAt.
func (op *releaseOp) deleteResourceAt(ref *structs.ReleaseResourceRef, index *uint64) error {
	if index != nil && !releaseDeletesChecked(ref.Type) {
		return fmt.Errorf("a %s can't be deleted with check-and-set", ref.Type)
	}
	if err := op.interruption(); err != nil {
		return err
	}
	srv := op.m.srv
	var generic structs.GenericResponse
	var err error
	switch ref.Type {
	case structs.ReleaseResourceNamespace:
		err = srv.RPC("Namespace.DeleteNamespaces", &structs.NamespaceDeleteRequest{
			Namespaces: []string{ref.Name}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceQuota:
		err = srv.RPC("Quota.DeleteQuotaSpecs", &structs.QuotaSpecDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceACLPolicy:
		err = srv.RPC("ACL.DeletePolicies", &structs.ACLPolicyDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceVariable:
		v := &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: ref.Namespace, Path: ref.Name}}
		varOp := structs.VarOpDelete
		if index != nil {
			varOp, v.ModifyIndex = structs.VarOpDeleteCAS, *index
		}
		var reply structs.VariablesApplyResponse
		err = srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
			Op: varOp, Var: v, WriteRequest: op.writeRequest(ref.Namespace),
		}, &reply)
		switch {
		case err != nil:
		case reply.Error != nil:
			err = reply.Error
		case reply.IsConflict():
			err = fmt.Errorf("the variable %q (namespace %q) %w", ref.Name, ref.Namespace, errReleaseResourceChanged)
		}
	case structs.ReleaseResourceObjectKind:
		group, kind, perr := structs.ParseObjectKindRef(ref.Name)
		if perr != nil {
			return perr
		}
		err = srv.RPC("ObjectKind.DeleteObjectKind", &structs.ObjectKindDeleteRequest{
			Group: group, Kind: kind, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceAdmissionPolicy:
		err = srv.RPC("AdmissionPolicy.DeleteAdmissionPolicies", &structs.AdmissionPolicyDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceExternalService:
		ns := ref.Namespace
		if ns == "" {
			ns = op.namespace
		}
		var reply structs.ExternalServiceDeleteResponse
		err = srv.RPC(structs.ServiceRegistrationDeleteExternalRPCMethod, &structs.ExternalServiceDeleteRequest{
			ID: ref.Name, WriteRequest: op.writeRequest(ns),
		}, &reply)
	case structs.ReleaseResourceObject:
		group, kind, name, perr := splitObjectRefName(ref.Name)
		if perr != nil {
			return perr
		}
		ns := ref.Namespace
		if ns == "" {
			ns = op.namespace
		}
		var reply structs.CustomObjectWriteResponse
		err = srv.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
			Group: group, Kind: kind, Name: name, CAS: index, WriteRequest: op.writeRequest(ns),
		}, &reply)
		if index != nil && structs.IsObjectConflictError(err) {
			err = &changedError{err}
		}
	case structs.ReleaseResourceJob:
		var reply structs.JobDeregisterResponse
		err = srv.RPC("Job.Deregister", &structs.JobDeregisterRequest{
			JobID: ref.Name, Purge: true, WriteRequest: op.writeRequest(ref.Namespace),
		}, &reply)
	default:
		return fmt.Errorf("unknown resource type %q", ref.Type)
	}
	if err == nil {
		return nil
	}
	// Whether the resource is gone is read from the state, never from the
	// text of the error: a refusal can quote names somebody else chose (a
	// job, a volume, a label), and the refusal of an unknown token says
	// "ACL token not found" (review finding JOG-6).
	snap, lookupErr := srv.State().Snapshot()
	if lookupErr != nil {
		return errors.Join(err, lookupErr)
	}
	live, lookupErr := op.m.readLive(snap, ref)
	switch {
	case lookupErr != nil:
		return errors.Join(err, lookupErr)
	case !live.exists():
		return nil
	}
	return err
}

// objectPrerequisiteMissing reports whether the kind of obj, or for a
// namespaced kind the namespace the object is written to (requested), is
// not in snap: a release can create both before the object.
func objectPrerequisiteMissing(snap *state.StateSnapshot, obj *structs.CustomObject, requested string) (bool, error) {
	kind, err := snap.ObjectKindByName(nil, obj.Group, obj.Kind)
	if err != nil {
		return false, err
	}
	if kind == nil {
		return true, nil
	}
	if !kind.Namespaced {
		return false, nil
	}
	ns, err := snap.NamespaceByName(nil, objectNamespace(kind, requested))
	if err != nil {
		return false, err
	}
	return ns == nil, nil
}

// resourceChanged reports whether applying res would change the live
// resource. Jobs and objects are compared by their endpoints (plan, dry run)
// instead.
func resourceChanged(res *structs.ReleaseResource, live *liveResource) bool {
	if !live.exists() {
		return true
	}
	desired := res.Copy()
	switch res.Type {
	case structs.ReleaseResourceQuota:
		desired.Quota.Canonicalize()
	case structs.ReleaseResourceAdmissionPolicy:
		desired.AdmissionPolicy.Canonicalize()
	case structs.ReleaseResourceObjectKind:
		if desired.ObjectKind.Columns == nil && len(live.res.ObjectKind.Columns) == 0 {
			desired.ObjectKind.Columns = live.res.ObjectKind.Columns
		}
		if desired.ObjectKind.Rules == nil && len(live.res.ObjectKind.Rules) == 0 {
			desired.ObjectKind.Rules = live.res.ObjectKind.Rules
		}
	case structs.ReleaseResourceNamespace:
		normalizeNamespace(desired.Namespace, live.res.Namespace)
	case structs.ReleaseResourceExternalService:
		// the datacenter of a service without one is the one of the node
		// that runs its checks
		if desired.ExternalService.Service.Datacenter == "" {
			desired.ExternalService.Service.Datacenter = live.res.ExternalService.Service.Datacenter
		}
	}
	return releaseResourceHash(desired) != live.hash
}

// normalizeNamespace fills in what the namespace endpoint would store for
// empty fields of desired, so that the comparison with the live namespace
// ignores them.
func normalizeNamespace(desired, live *structs.Namespace) {
	if desired.Capabilities == nil {
		desired.Capabilities = live.Capabilities
	}
	if desired.NodePoolConfiguration == nil {
		desired.NodePoolConfiguration = live.NodePoolConfiguration
	}
}
