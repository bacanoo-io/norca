// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package secretengine holds the backends of the dynamic secrets engine
// (norca-doc content/docs/design/secrets-plan.md, "Rotation and dynamic
// secrets"). A backend creates credentials for the roles of a SecretEngine,
// extends them and brings the target system in line with the revoked leases.
// It keeps no state: everything it needs comes from the engine object and the
// leases in Raft, so any leader can take over.
//
// Credentials only pass through memory here; they are never logged and never
// part of an error.
package secretengine

import (
	"context"
	"fmt"
	"time"

	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

// Backend is a backend of the dynamic secrets engine.
type Backend interface {
	// Type is the engine type the backend serves.
	Type() string

	// Issue creates a credential for a role.
	Issue(ctx context.Context, req *IssueRequest) (*Credential, error)

	// Renew extends a credential.
	Renew(ctx context.Context, req *RenewRequest) (*RenewResult, error)

	// Sync brings the target system in line with the revoked leases.
	Sync(ctx context.Context, req *SyncRequest) error
}

// IssueRequest asks for a credential of a role.
type IssueRequest struct {
	Engine *structs.SecretEngine
	Role   *structs.SecretEngineRole

	// MaxExpireTime is the end of the role's max_ttl.
	MaxExpireTime time.Time

	// LeaseID and the task the credential is for, and the call of the
	// operator.
	LeaseID   string
	Params    map[string]string
	JobID     string
	TaskGroup string
	Task      string
	AllocID   string
	Call      OperatorCall
}

// Credential is a created credential. Items must never be logged.
type Credential struct {
	Items structs.SecretItems

	// BackendRef identifies the credential at the backend; it is not secret.
	BackendRef string

	// Expires is when the credential ends, if the backend decides; a lease
	// ends with it at the latest.
	Expires time.Time

	// NotRenewable means the credential can't be extended: the lease ends
	// with it and a new one is requested in time.
	NotRenewable bool
}

// SyncRequest is the state the backend is brought in line with.
type SyncRequest struct {
	Engine *structs.SecretEngine

	// Revoked are the backend references of the revoked leases whose
	// credentials have not expired yet, with their revocation times.
	Revoked map[string]time.Time

	// Call is the call of the operator.
	Call OperatorCall
}

// Backends are the backends by engine type.
var Backends = map[string]Backend{
	structs.SecretEngineTypeOperator: &Operator{},
}

// For returns the backend of an engine type.
func For(engineType string) (Backend, error) {
	if b, ok := Backends[engineType]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("unsupported secret engine type %q", engineType)
}
