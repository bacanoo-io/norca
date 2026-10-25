// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
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
	hash        string
}

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
		live.modifyIndex = ns.ModifyIndex
	case structs.ReleaseResourceQuota:
		q, err := snap.QuotaSpecByName(nil, ref.Name)
		if err != nil || q == nil {
			return live, err
		}
		res.Quota = q.Copy()
		live.modifyIndex = q.ModifyIndex
	case structs.ReleaseResourceACLPolicy:
		p, err := snap.ACLPolicyByName(nil, ref.Name)
		if err != nil || p == nil {
			return live, err
		}
		cp := *p
		res.ACLPolicy = &cp
		live.modifyIndex = p.ModifyIndex
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
		live.modifyIndex = v.ModifyIndex
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
		live.modifyIndex = k.ModifyIndex
	case structs.ReleaseResourceAdmissionPolicy:
		p, err := snap.AdmissionPolicyByName(nil, ref.Name)
		if err != nil || p == nil {
			return live, err
		}
		res.AdmissionPolicy = p.Copy()
		live.modifyIndex = p.ModifyIndex
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
		live.modifyIndex = o.ModifyIndex
		live.version = o.Generation
	case structs.ReleaseResourceJob:
		j, err := snap.JobByID(nil, ref.Namespace, ref.Name)
		if err != nil || j == nil {
			return live, err
		}
		res.Job = j.Copy()
		live.modifyIndex = j.JobModifyIndex
		live.version = j.Version
	default:
		return nil, fmt.Errorf("unknown resource type %q", ref.Type)
	}
	live.res = res
	live.hash = releaseResourceHash(res)
	return live, nil
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
		v = q
	case structs.ReleaseResourceACLPolicy:
		p := *res.ACLPolicy
		p.CreateIndex, p.ModifyIndex, p.Hash = 0, 0, nil
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
}

// applyResource creates or updates a resource through the endpoint of its
// type.
func (op *releaseOp) applyResource(res *structs.ReleaseResource) (*applyResult, error) {
	srv := op.m.srv
	out := &applyResult{}
	var generic structs.GenericResponse
	switch res.Type {
	case structs.ReleaseResourceNamespace:
		ns := res.Namespace.Copy()
		ns.CreateIndex, ns.ModifyIndex, ns.Hash = 0, 0, nil
		return out, srv.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
			Namespaces: []*structs.Namespace{ns}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceQuota:
		q := res.Quota.Copy()
		q.CreateIndex, q.ModifyIndex, q.Hash = 0, 0, nil
		return out, srv.RPC("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
			Quotas: []*structs.QuotaSpec{q}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceACLPolicy:
		p := *res.ACLPolicy
		p.CreateIndex, p.ModifyIndex, p.Hash = 0, 0, nil
		return out, srv.RPC("ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
			Policies: []*structs.ACLPolicy{&p}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceVariable:
		v := res.Variable.Copy()
		v.CreateIndex, v.ModifyIndex, v.CreateTime, v.ModifyTime = 0, 0, 0, 0
		var reply structs.VariablesApplyResponse
		err := srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
			Op: structs.VarOpSet, Var: &v, WriteRequest: op.writeRequest(v.Namespace),
		}, &reply)
		if err == nil && reply.Error != nil {
			err = reply.Error
		}
		return out, err
	case structs.ReleaseResourceObjectKind:
		k := res.ObjectKind.Copy()
		k.CreateIndex, k.ModifyIndex = 0, 0
		var reply structs.ObjectKindUpsertResponse
		return out, srv.RPC("ObjectKind.UpsertObjectKind", &structs.ObjectKindUpsertRequest{
			ObjectKind: k, WriteRequest: op.writeRequest(""),
		}, &reply)
	case structs.ReleaseResourceAdmissionPolicy:
		p := res.AdmissionPolicy.Copy()
		p.CreateIndex, p.ModifyIndex = 0, 0
		return out, srv.RPC("AdmissionPolicy.UpsertAdmissionPolicies", &structs.AdmissionPolicyUpsertRequest{
			AdmissionPolicies: []*structs.AdmissionPolicy{p}, WriteRequest: op.writeRequest(""),
		}, &generic)
	case structs.ReleaseResourceObject:
		o := res.Object.Copy()
		ns := o.Namespace
		if ns == "" {
			ns = op.namespace
		}
		var reply structs.CustomObjectWriteResponse
		return out, srv.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
			Object: o, WriteRequest: op.writeRequest(ns),
		}, &reply)
	case structs.ReleaseResourceJob:
		j := res.Job.Copy()
		j.CreateIndex, j.ModifyIndex, j.JobModifyIndex = 0, 0, 0
		j.Version, j.Stable, j.SubmitTime = 0, false, 0
		j.Status, j.StatusDescription = "", ""
		var sub *structs.JobSubmission
		if res.Submission != nil {
			sub = res.Submission.Copy()
		}
		var reply structs.JobRegisterResponse
		err := srv.RPC("Job.Register", &structs.JobRegisterRequest{
			Job: j, Submission: sub, WriteRequest: op.writeRequest(j.Namespace),
		}, &reply)
		out.evalID, out.warnings = reply.EvalID, reply.Warnings
		return out, err
	}
	return out, fmt.Errorf("unknown resource type %q", res.Type)
}

// deleteResource deletes a resource through the endpoint of its type. A
// resource that doesn't exist is not an error.
func (op *releaseOp) deleteResource(ref *structs.ReleaseResourceRef) error {
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
		var reply structs.VariablesApplyResponse
		err = srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
			Op:           structs.VarOpDelete,
			Var:          &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: ref.Namespace, Path: ref.Name}},
			WriteRequest: op.writeRequest(ref.Namespace),
		}, &reply)
		if err == nil && reply.Error != nil {
			err = reply.Error
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
			Group: group, Kind: kind, Name: name, WriteRequest: op.writeRequest(ns),
		}, &reply)
	case structs.ReleaseResourceJob:
		var reply structs.JobDeregisterResponse
		err = srv.RPC("Job.Deregister", &structs.JobDeregisterRequest{
			JobID: ref.Name, Purge: true, WriteRequest: op.writeRequest(ref.Namespace),
		}, &reply)
	default:
		return fmt.Errorf("unknown resource type %q", ref.Type)
	}
	if isNotFoundError(err) {
		return nil
	}
	return err
}

// isNotFoundError reports whether err says that the resource doesn't exist.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if code, _, ok := structs.CodeFromRPCCodedErr(err); ok && code == http.StatusNotFound {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "unknown job") || strings.Contains(msg, "nonexistent")
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
