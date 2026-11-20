// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// The checked writes of namespaces, quotas, ACL policies, ACL roles and
// network policies: a check-and-set on the modify index the caller read.
// Index 0 creates a resource that must not exist yet; any other index writes
// only while the stored resource still has it. A write that fails the check
// writes nothing and returns an error wrapping ErrIndexConflict: read the
// resource again and decide. Namespaces, quotas, ACL policies and ACL roles
// are written in the authoritative region, so the index is the one it
// reports; network policies are regional.

// ErrIndexConflict is wrapped by the errors of checked writes that failed
// because the resource was created or changed since it was read (409
// Conflict).
var ErrIndexConflict = errors.New("resource modified concurrently")

// checkedQuery returns the query of a checked write expecting index.
func checkedQuery(index uint64) string {
	return "?cas=" + strconv.FormatUint(index, 10)
}

// checkedWriteError wraps a 409 Conflict response in ErrIndexConflict.
func checkedWriteError(err error) error {
	var respErr UnexpectedResponseError
	if errors.As(err, &respErr) && respErr.StatusCode() == http.StatusConflict {
		return fmt.Errorf("%w: %w", ErrIndexConflict, err)
	}
	return err
}

// CheckedRegister registers a namespace only while its modify index is
// index; 0 creates a namespace that must not exist yet.
func (n *Namespaces) CheckedRegister(namespace *Namespace, index uint64, q *WriteOptions) (*WriteMeta, error) {
	if namespace == nil || namespace.Name == "" {
		return nil, errors.New("missing namespace name")
	}
	wm, err := n.client.put("/v1/namespace"+checkedQuery(index), namespace, nil, q)
	if err != nil {
		return nil, checkedWriteError(err)
	}
	return wm, nil
}

// CheckedRegister registers a quota specification only while its modify
// index is index; 0 creates a quota that must not exist yet.
func (q *Quotas) CheckedRegister(spec *QuotaSpec, index uint64, qo *WriteOptions) (*WriteMeta, error) {
	if spec == nil || spec.Name == "" {
		return nil, errors.New("missing quota name")
	}
	wm, err := q.client.put("/v1/quota"+checkedQuery(index), spec, nil, qo)
	if err != nil {
		return nil, checkedWriteError(err)
	}
	return wm, nil
}

// CheckedUpsert writes an ACL policy only while its modify index is index;
// 0 creates a policy that must not exist yet.
func (a *ACLPolicies) CheckedUpsert(policy *ACLPolicy, index uint64, q *WriteOptions) (*WriteMeta, error) {
	if policy == nil || policy.Name == "" {
		return nil, errors.New("missing policy name")
	}
	wm, err := a.client.put("/v1/acl/policy/"+url.PathEscape(policy.Name)+checkedQuery(index), policy, nil, q)
	if err != nil {
		return nil, checkedWriteError(err)
	}
	return wm, nil
}

// CheckedUpsert writes an ACL role only while its modify index is index: a
// role with an ID is changed, and the index is checked against the stored
// role of that ID; a role without an ID is created, with index 0, while no
// role has its name.
func (a *ACLRoles) CheckedUpsert(role *ACLRole, index uint64, w *WriteOptions) (*ACLRole, *WriteMeta, error) {
	if role == nil || role.Name == "" {
		return nil, nil, errors.New("missing ACL role name")
	}
	path := "/v1/acl/role"
	if role.ID != "" {
		path += "/" + url.PathEscape(role.ID)
	}
	var resp ACLRole
	wm, err := a.client.put(path+checkedQuery(index), role, &resp, w)
	if err != nil {
		return nil, nil, checkedWriteError(err)
	}
	return &resp, wm, nil
}

// CheckedRegister writes a network policy only while its modify index is
// index; 0 creates a policy that must not exist yet. The namespace is the
// one Register uses.
func (n *NetworkPolicies) CheckedRegister(policy *NetworkPolicy, index uint64, w *WriteOptions) (*WriteMeta, error) {
	wm, err := n.register(policy, checkedQuery(index), w)
	if err != nil {
		return nil, checkedWriteError(err)
	}
	return wm, nil
}
