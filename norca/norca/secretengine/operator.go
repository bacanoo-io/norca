// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The calls of the operator contract.
const (
	OperatorPathIssue  = "/issue"
	OperatorPathRenew  = "/renew"
	OperatorPathRevoke = "/revoke"
)

// MaxOperatorItems bounds the items of a credential: every answer of an
// operator goes through the leader.
const MaxOperatorItems = 64 << 10

// OperatorCall posts a call of the operator contract to the operator of an
// engine and decodes its answer. The server supplies it: it finds the
// instances of the service and holds the certificates.
type OperatorCall func(ctx context.Context, path string, in, out any) error

// OperatorIssueRequest is the body of the issue call. Issuing is idempotent
// by the lease ID: the leader may repeat a call after a failure.
type OperatorIssueRequest struct {
	Namespace string
	Engine    string
	Role      string

	// Config is the configuration of the role, opaque to Norca.
	Config map[string]string `json:",omitempty"`

	// Params are the parameters of the job's secret block. Their keys are
	// allowed by the role; the operator checks their values against it.
	Params map[string]string `json:",omitempty"`

	LeaseID string

	// The task the credential is for.
	JobID     string
	TaskGroup string
	Task      string
	AllocID   string

	// TTL is how long the lease lasts without a renewal.
	TTL time.Duration

	// MaxExpire is when the lease ends at the latest.
	MaxExpire time.Time
}

// OperatorIssueResponse is the answer of the issue call. Items must never be
// logged.
type OperatorIssueResponse struct {
	Items structs.SecretItems

	// Ref identifies the credential at the operator; it is not secret. The
	// lease ID is used without one.
	Ref string

	// Expires is when the credential ends; zero if the lease decides.
	Expires time.Time

	// Renewable false means the credential can't be extended, for example a
	// certificate: the task gets a new one in time.
	Renewable *bool `json:",omitempty"`
}

// OperatorRenewRequest is the body of the renew call.
type OperatorRenewRequest struct {
	LeaseID string
	Ref     string
	TTL     time.Duration
}

// OperatorRenewResponse is the answer of the renew call.
type OperatorRenewResponse struct {
	// Expires is when the credential ends; zero if the lease decides.
	Expires time.Time

	// Items are new values, if the renewal replaced the credential. They
	// reach the task like a new version of the secret. Must never be logged.
	Items structs.SecretItems `json:",omitempty"`
}

// OperatorRevokeRequest is the body of the revoke call: every revoked
// credential that has not expired yet, by its Ref, or by the lease ID when
// the lease has no Ref because its issue did not finish. It is sent again
// with each change and periodically, so it must be idempotent, and it must
// accept unknown references.
type OperatorRevokeRequest struct {
	Namespace string
	Engine    string
	Revoked   map[string]time.Time
}

// RenewRequest asks a backend to extend a credential.
type RenewRequest struct {
	Engine *structs.SecretEngine
	Lease  *structs.SecretLease
	Call   OperatorCall
}

// RenewResult is the outcome of a renewal.
type RenewResult struct {
	// Expires is when the credential ends; zero if the lease decides.
	Expires time.Time

	// Items are the new values of a replaced credential.
	Items structs.SecretItems
}

// Operator is the backend of the engines served by an operator: a service
// that creates the credentials of its target system. It keeps no state; the
// admin credential of the target system belongs to the operator.
type Operator struct{}

// Type returns the engine type.
func (*Operator) Type() string { return structs.SecretEngineTypeOperator }

// Issue asks the operator for a credential.
func (*Operator) Issue(ctx context.Context, req *IssueRequest) (*Credential, error) {
	if req.Call == nil {
		return nil, errors.New("no connection to the operator")
	}
	var out OperatorIssueResponse
	err := req.Call(ctx, OperatorPathIssue, &OperatorIssueRequest{
		Namespace: req.Engine.Namespace,
		Engine:    req.Engine.Name,
		Role:      req.Role.Name,
		Config:    req.Role.Config,
		Params:    req.Params,
		LeaseID:   req.LeaseID,
		JobID:     req.JobID,
		TaskGroup: req.TaskGroup,
		Task:      req.Task,
		AllocID:   req.AllocID,
		TTL:       req.Role.TTL,
		MaxExpire: req.MaxExpireTime,
	}, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, errors.New("the operator returned no items")
	}
	if err := checkOperatorItems(out.Items); err != nil {
		return nil, err
	}
	ref := out.Ref
	if ref == "" {
		ref = req.LeaseID
	}
	return &Credential{Items: out.Items, BackendRef: ref, Expires: out.Expires,
		NotRenewable: out.Renewable != nil && !*out.Renewable}, nil
}

// Sync sends the revoked credentials to the operator in one call.
func (*Operator) Sync(ctx context.Context, req *SyncRequest) error {
	if len(req.Revoked) == 0 {
		return nil
	}
	if req.Call == nil {
		return errors.New("no connection to the operator")
	}
	return req.Call(ctx, OperatorPathRevoke, &OperatorRevokeRequest{
		Namespace: req.Engine.Namespace,
		Engine:    req.Engine.Name,
		Revoked:   req.Revoked,
	}, nil)
}

// Renew asks the operator to extend a credential.
func (*Operator) Renew(ctx context.Context, req *RenewRequest) (*RenewResult, error) {
	if req.Call == nil {
		return nil, errors.New("no connection to the operator")
	}
	var out OperatorRenewResponse
	err := req.Call(ctx, OperatorPathRenew, &OperatorRenewRequest{
		LeaseID: req.Lease.ID, Ref: req.Lease.BackendRef, TTL: req.Lease.TTL}, &out)
	if err != nil {
		return nil, err
	}
	if err := checkOperatorItems(out.Items); err != nil {
		return nil, err
	}
	return &RenewResult{Expires: out.Expires, Items: out.Items}, nil
}

// checkOperatorItems bounds the items of an answer.
func checkOperatorItems(items structs.SecretItems) error {
	size := 0
	for k, v := range items {
		size += len(k) + len(v)
	}
	if size > MaxOperatorItems {
		return fmt.Errorf("the items of the operator are larger than %d bytes", MaxOperatorItems)
	}
	return nil
}
