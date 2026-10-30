// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package secrets is the client side of secret delivery
// (norca-doc content/docs/design/secrets-plan.md, "Delivery to tasks", "Provider resolution
// and fallback" and "External providers"): the provider interface, the
// built-in providers ("norca", the built-in store over server RPC, and
// "vault", the agent's existing Vault integration), the resolution of the
// provider that answers a lookup, and the file formats of delivered secrets.
//
// Values only ever live in memory here; they are handed to the task as files
// in its secrets directory or as environment variables, never logged and
// never written to the client's state.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Identity is the workload a lookup is made for. Providers authenticate
// with it: the built-in store and identity federated providers with the
// signed workload identity, the existing Vault integration with the task's
// server derived Vault token.
type Identity struct {
	// Token is the task's signed workload identity.
	Token string

	Namespace string
	JobID     string
	Group     string
	Task      string
	AllocID   string

	// VaultToken and VaultNamespace are the task's Vault token and
	// namespace from the existing Vault integration, if the task has a
	// vault block.
	VaultToken     string
	VaultNamespace string
}

// Request is a lookup of one secret.
type Request struct {
	// Path is the provider-specific path, placeholders already replaced.
	Path string

	// Version pins a version; 0 is the latest. Providers without versions
	// reject pinned versions.
	Version uint64

	// Identity is the workload the lookup is made for.
	Identity *Identity

	// Engine and Role are reserved for dynamic secrets: a provider with the
	// Leases capability creates a credential for the role and returns it
	// with a lease.
	Engine string
	Role   string
}

// Secret is a fetched secret value. It must never be logged.
type Secret struct {
	// Items are the key/value pairs of the secret.
	Items map[string]string

	// Version identifies the value at the provider; a change of the value
	// changes it. It is not secret (shown in task events).
	Version string

	// Lease is set for dynamic secrets, which must be renewed and are
	// revoked when the task stops.
	Lease *Lease

	// Metadata is the metadata of the secret, if the provider has it. It
	// is not secret; responses in the shapes of other stores (the Vault KV
	// v2 metadata) are built from it.
	Metadata *Metadata
}

// Metadata is the metadata of a secret and of its fetched version. It never
// contains values.
type Metadata struct {
	// Version is the fetched version, CreatedTime the time it was written.
	Version     uint64
	CreatedTime time.Time

	// CurrentVersion is the latest version of the secret, OldestVersion
	// the oldest retained one.
	CurrentVersion uint64
	OldestVersion  uint64
	MaxVersions    int

	// CreateTime and UpdateTime are the times the secret was created and
	// last changed.
	CreateTime time.Time
	UpdateTime time.Time

	// Deleted is set when the current version is soft deleted or the
	// secret destroyed.
	Deleted bool

	// Labels are the free labels of the secret.
	Labels map[string]string

	// Versions are the retained versions, oldest first.
	Versions []*VersionMetadata
}

// VersionMetadata is the metadata of one version of a secret.
type VersionMetadata struct {
	Version     uint64
	CreatedTime time.Time
	Deleted     bool
	Destroyed   bool
}

// MetadataReader is implemented by providers that can read the metadata of
// a secret without its value.
type MetadataReader interface {
	ReadMetadata(ctx context.Context, req *Request) (*Metadata, error)
}

// Lease is the lease of a dynamic secret.
type Lease struct {
	ID        string
	Duration  time.Duration
	Renewable bool
}

// Capabilities describe what a provider supports, so callers can use the
// optional interfaces and report clear errors for unsupported features.
type Capabilities struct {
	// Versions: pinned versions can be fetched.
	Versions bool

	// Watch: the provider implements Watcher and notifies about changes;
	// otherwise changes are found by fetching every PollInterval.
	Watch bool

	// PollInterval is the interval of change detection by fetching, for
	// providers without Watch.
	PollInterval time.Duration

	// Leases: the provider returns dynamic secrets with leases and
	// implements LeaseManager.
	Leases bool

	// AuditsReads: the provider records an audit record for every value
	// read itself (as the built-in store does on the servers); the client
	// then records none, so each read is audited exactly once.
	AuditsReads bool
}

// Provider answers secret lookups.
type Provider interface {
	// Name is the name lookups and routes refer to ("norca", "vault").
	Name() string

	// Type is the kind of provider (for example "norca", "vault",
	// "openbao", "aws-secretsmanager", "plugin").
	Type() string

	// Capabilities returns what the provider supports.
	Capabilities() Capabilities

	// Fetch reads the value of a secret.
	Fetch(ctx context.Context, req *Request) (*Secret, error)
}

// Watcher is implemented by providers that can block until a secret
// changes, so that values are fetched only when they change.
type Watcher interface {
	// Watch blocks until the version of the secret differs from version and
	// returns the new version, or until ctx is done. It never returns
	// values.
	Watch(ctx context.Context, req *Request, version string) (string, error)
}

// LeaseManager is implemented by providers of dynamic secrets.
type LeaseManager interface {
	Renew(ctx context.Context, lease *Lease, identity *Identity) (*Lease, error)
	Revoke(ctx context.Context, lease *Lease, identity *Identity) error
}

// ErrorKind classifies provider errors.
type ErrorKind int

const (
	// KindUnavailable: the provider can't be reached or is sealed. Lookups
	// are retried until the timeout.
	KindUnavailable ErrorKind = iota

	// KindNotFound: the secret (or version) does not exist. Lookups are
	// retried until the timeout, as a secret may be written after the job.
	KindNotFound

	// KindDenied: the identity may not read the secret.
	KindDenied

	// KindInvalid: the request can't be served (invalid path, unsupported
	// feature, missing configuration).
	KindInvalid
)

func (k ErrorKind) String() string {
	switch k {
	case KindUnavailable:
		return "unavailable"
	case KindNotFound:
		return "not found"
	case KindDenied:
		return "permission denied"
	default:
		return "invalid"
	}
}

// Error is a provider error. It never contains a value.
type Error struct {
	Provider string
	Kind     ErrorKind
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider %s: %v", e.Provider, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Recoverable reports whether the lookup may succeed when retried.
func (e *Error) Recoverable() bool {
	return e.Kind == KindUnavailable || e.Kind == KindNotFound
}

// NewError returns a provider error.
func NewError(provider string, kind ErrorKind, err error) *Error {
	return &Error{Provider: provider, Kind: kind, Err: err}
}

// IsRecoverable reports whether a lookup that failed with err may succeed
// when retried. Errors that are not provider errors are not.
func IsRecoverable(err error) bool {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Recoverable()
	}
	return false
}

// KindOf returns the kind of a provider error; KindInvalid for other errors.
func KindOf(err error) ErrorKind {
	var perr *Error
	if errors.As(err, &perr) {
		return perr.Kind
	}
	return KindInvalid
}
