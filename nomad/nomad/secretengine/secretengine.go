// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package secretengine holds the backends of the built-in dynamic secrets
// engine (norca-doc content/docs/design/secrets-plan.md, "Rotation and
// dynamic secrets"). A backend creates credentials for the roles of a
// SecretEngine, brings the backend in line with the leases (revocations and
// admin keys) and rotates its admin key. It keeps no state: everything it
// needs comes from the engine object, the engine's admin credential (a
// built-in secret, read by the leader) and the leases in Raft, so any leader
// can take over.
//
// Credentials and admin credentials only pass through memory here; they are
// never logged and never part of an error.
package secretengine

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Backend is a backend of the dynamic secrets engine.
type Backend interface {
	// Type is the engine type the backend serves.
	Type() string

	// Validate checks the engine's configuration and its admin credential.
	Validate(engine *structs.SecretEngine, credential structs.SecretItems) error

	// Issue creates a credential for a role.
	Issue(ctx context.Context, req *IssueRequest) (*Credential, error)

	// Sync brings the backend in line with the engine and its leases: the
	// admin keys (the current one, the retired ones still signing leases)
	// and the revocations of the revoked leases.
	Sync(ctx context.Context, req *SyncRequest) error

	// RotateRoot creates a new admin key, makes the backend accept it next
	// to the current one, and returns the new admin credential. The caller
	// stores it and retires the old key.
	RotateRoot(ctx context.Context, req *SyncRequest) (*RotateResult, error)
}

// IssueRequest asks for a credential of a role.
type IssueRequest struct {
	Engine     *structs.SecretEngine
	Role       *structs.SecretEngineRole
	Credential structs.SecretItems

	// Name describes the credential at the backend (not secret).
	Name string

	// IssueTime is the creation time, MaxExpireTime the end of the role's
	// max_ttl, when the credential itself expires.
	IssueTime     time.Time
	MaxExpireTime time.Time
}

// Credential is a created credential. Items must never be logged.
type Credential struct {
	Items structs.SecretItems

	// BackendRef identifies the credential at the backend, SigningKey the
	// admin key that signed it; neither is secret.
	BackendRef string
	SigningKey string

	// File and FileItem are the default delivery of the credential: the
	// file name below secrets/ and the item written to it.
	File     string
	FileItem string
}

// SyncRequest is the state the backend is brought in line with.
type SyncRequest struct {
	Engine     *structs.SecretEngine
	Credential structs.SecretItems

	// Revoked are the backend references of the revoked leases whose
	// credentials have not expired yet, with their revocation times.
	Revoked map[string]time.Time
}

// RotateResult is the outcome of a rotate-root.
type RotateResult struct {
	// Credential is the new admin credential.
	Credential structs.SecretItems

	// OldKey and NewKey are the public keys of the replaced and the new
	// admin key.
	OldKey string
	NewKey string
}

// Backends are the backends by engine type.
var Backends = map[string]Backend{
	structs.SecretEngineTypeNATS: &NATS{},
}

// For returns the backend of an engine type.
func For(engineType string) (Backend, error) {
	if b, ok := Backends[engineType]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("unsupported secret engine type %q", engineType)
}
