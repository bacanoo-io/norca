// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	multierror "github.com/hashicorp/go-multierror"
)

// Dynamic secrets of the built-in engine (norca-doc
// content/docs/design/secrets-plan.md, "Rotation and dynamic secrets",
// phase 7). A SecretEngine of a namespace names the operator, a service that
// creates the credentials of its target system, and the roles credentials are
// created for. The admin credential of the target system belongs to the
// operator. A task asks for a credential with a secret block naming the engine
// and the role; the leader records a SecretLease in Raft, asks the operator
// and returns the credential to the client only. Leases hold the operator's
// reference of the credential, never the credential itself.

const (
	// SecretEngineListRPCMethod lists the engines of a namespace.
	//
	// Args: SecretEngineListRequest
	// Reply: SecretEngineListResponse
	SecretEngineListRPCMethod = "SecretEngine.List"

	// SecretEngineReadRPCMethod reads an engine.
	//
	// Args: SecretEngineReadRequest
	// Reply: SecretEngineResponse
	SecretEngineReadRPCMethod = "SecretEngine.Read"

	// SecretEngineUpsertRPCMethod creates or updates an engine.
	//
	// Args: SecretEngineUpsertRequest
	// Reply: SecretEngineWriteResponse
	SecretEngineUpsertRPCMethod = "SecretEngine.Upsert"

	// SecretEngineDeleteRPCMethod deletes an engine without live leases.
	//
	// Args: SecretEngineDeleteRequest
	// Reply: SecretEngineWriteResponse
	SecretEngineDeleteRPCMethod = "SecretEngine.Delete"

	// SecretLeaseListRPCMethod lists the leases of a namespace.
	//
	// Args: SecretLeaseListRequest
	// Reply: SecretLeaseListResponse
	SecretLeaseListRPCMethod = "SecretLease.List"

	// SecretLeaseReadRPCMethod reads a lease.
	//
	// Args: SecretLeaseReadRequest
	// Reply: SecretLeaseResponse
	SecretLeaseReadRPCMethod = "SecretLease.Read"

	// SecretLeaseCreateRPCMethod creates a credential for a task. It is
	// authenticated with the task's workload identity and executed by the
	// leader; the credential is in the reply only.
	//
	// Args: SecretLeaseCreateRequest
	// Reply: SecretLeaseCreateResponse
	SecretLeaseCreateRPCMethod = "SecretLease.Create"

	// SecretLeaseRenewRPCMethod extends a lease of the task, up to its
	// max_ttl.
	//
	// Args: SecretLeaseRenewRequest
	// Reply: SecretLeaseWriteResponse
	SecretLeaseRenewRPCMethod = "SecretLease.Renew"

	// SecretLeaseRevokeRPCMethod revokes a lease (by its task or an
	// operator) or all leases of an engine (by an operator).
	//
	// Args: SecretLeaseRevokeRequest
	// Reply: SecretLeaseRevokeResponse
	SecretLeaseRevokeRPCMethod = "SecretLease.Revoke"
)

const (
	// SecretEngineTypeOperator is an engine served by an operator: a service
	// that creates the credentials of its target system. Norca keeps the
	// leases; the admin credential belongs to the operator.
	SecretEngineTypeOperator = "operator"

	// SecretLeaseDefaultTTL and SecretLeaseDefaultMaxTTL are the lease
	// durations of roles that set none.
	SecretLeaseDefaultTTL    = time.Hour
	SecretLeaseDefaultMaxTTL = 24 * time.Hour

	// SecretLeaseMinTTL bounds the lease durations from below.
	SecretLeaseMinTTL = time.Second

	// secretEngineCredsPrefix is the first segment of the virtual secret
	// paths of credentials.
	secretEngineCredsPrefix = "engines/"
)

// The operations of the audit records of the engines and leases.
const (
	SecretOpEngineWrite  = "engine-write"
	SecretOpEngineDelete = "engine-delete"
	SecretOpLeaseCreate  = "lease-create"
	SecretOpLeaseRenew   = "lease-renew"
	SecretOpLeaseRevoke  = "lease-revoke"
)

// The event types of the engines and leases on the Secret topic.
const (
	TypeSecretEngineUpserted = "SecretEngineUpserted"
	TypeSecretEngineDeleted  = "SecretEngineDeleted"
	TypeSecretLeaseCreated   = "SecretLeaseCreated"
	TypeSecretLeaseRenewed   = "SecretLeaseRenewed"
	TypeSecretLeaseRevoked   = "SecretLeaseRevoked"
	TypeSecretLeaseDeleted   = "SecretLeaseDeleted"
)

// IsSecretEngineEventType reports whether an event type of the Secret topic
// is about an engine or a lease, whose events are visible with
// read-secret-engine instead of list-secrets.
func IsSecretEngineEventType(typ string) bool {
	switch typ {
	case TypeSecretEngineUpserted, TypeSecretEngineDeleted, TypeSecretLeaseCreated,
		TypeSecretLeaseRenewed, TypeSecretLeaseRevoked, TypeSecretLeaseDeleted:
		return true
	}
	return false
}

// The reasons a lease was revoked.
const (
	// SecretLeaseRevokeStopped: the task stopped (revoked by its client) or
	// its allocation became terminal (revoked by the leader).
	SecretLeaseRevokeStopped = "allocation-stopped"

	// SecretLeaseRevokeExpired: the lease was not renewed before it expired.
	SecretLeaseRevokeExpired = "expired"

	// SecretLeaseRevokeMaxTTL: the lease reached its max_ttl.
	SecretLeaseRevokeMaxTTL = "max-ttl"

	// SecretLeaseRevokeOperator: an operator revoked the lease.
	SecretLeaseRevokeOperator = "operator"
)

var (
	// ErrSecretEngineNotFound is returned for an engine name without an
	// engine.
	ErrSecretEngineNotFound = errors.New("secret engine not found")

	// ErrSecretLeaseNotFound is returned for an unknown lease ID.
	ErrSecretLeaseNotFound = errors.New("secret lease not found")

	// ErrSecretLeaseRevoked is returned for a lease that was revoked or
	// expired; a revoked lease is never live again.
	ErrSecretLeaseRevoked = errors.New("secret lease is revoked")

	// validSecretEngineName restricts engine and role names.
	validSecretEngineName = regexp.MustCompile("^[a-zA-Z0-9_-]{1,64}$")
)

// SecretEngineCredsPath is the virtual secret path of the credentials of a
// role of an engine. Requesting a credential needs read-secret on it: from a
// policy bound to the job, or from a secret_access binding of the job.
func SecretEngineCredsPath(engine, role string) string {
	return secretEngineCredsPrefix + engine + "/creds/" + role
}

// ValidateSecretEngineName checks an engine or role name.
func ValidateSecretEngineName(kind, name string) error {
	if !validSecretEngineName.MatchString(name) {
		return fmt.Errorf("invalid %s name %q", kind, name)
	}
	return nil
}

// SecretEngine is a dynamic secrets engine of a namespace: the backend
// connection, the reference to its admin credential, and the roles.
type SecretEngine struct {
	Namespace string
	Name      string

	// Type is the backend type, "operator".
	Type string

	Description string

	// Operator is the service of an operator engine.
	Operator *SecretEngineOperator `json:",omitempty"`

	// Roles are the roles credentials are created for.
	Roles []*SecretEngineRole

	CreateIndex uint64
	ModifyIndex uint64
	CreateTime  int64
	ModifyTime  int64
}

// SecretEngineOperator names the service of the operator of an engine.
type SecretEngineOperator struct {
	// Service is the service of the operator in the native catalog.
	Service string

	// Namespace is the namespace of the service, by default the namespace
	// of the engine.
	Namespace string `json:",omitempty"`
}

// SecretEngineRole is a role of an engine: what the credentials of the role
// may do and how long their leases last.
type SecretEngineRole struct {
	Name string

	// TTL is the lease duration; a renewal extends the lease by TTL, up to
	// MaxTTL after the creation of the credential.
	TTL    time.Duration
	MaxTTL time.Duration

	// Config is passed to the operator of an engine; Norca does not read
	// it. It must hold nothing secret: it is stored in Raft.
	Config map[string]string `json:",omitempty"`

	// Params are the keys of the params a job may send to the operator.
	Params []string `json:",omitempty"`
}

// Copy returns a deep copy of the engine.
func (e *SecretEngine) Copy() *SecretEngine {
	if e == nil {
		return nil
	}
	out := *e
	if e.Operator != nil {
		o := *e.Operator
		out.Operator = &o
	}
	if e.Roles != nil {
		out.Roles = make([]*SecretEngineRole, len(e.Roles))
		for i, r := range e.Roles {
			out.Roles[i] = r.Copy()
		}
	}
	return &out
}

// Role returns the role with the name, or nil.
func (e *SecretEngine) Role(name string) *SecretEngineRole {
	for _, r := range e.Roles {
		if r != nil && r.Name == name {
			return r
		}
	}
	return nil
}

// Stub returns the list form of the engine.
func (e *SecretEngine) Stub() *SecretEngineStub {
	stub := &SecretEngineStub{
		Namespace:   e.Namespace,
		Name:        e.Name,
		Type:        e.Type,
		Description: e.Description,
		Roles:       []string{},
		CreateIndex: e.CreateIndex,
		ModifyIndex: e.ModifyIndex,
		CreateTime:  e.CreateTime,
		ModifyTime:  e.ModifyTime,
	}
	for _, r := range e.Roles {
		if r != nil {
			stub.Roles = append(stub.Roles, r.Name)
		}
	}
	return stub
}

// GetNamespace returns the engine's namespace. Used for pagination.
func (e *SecretEngine) GetNamespace() string { return e.Namespace }

// GetID returns the engine's name. Used for pagination.
func (e *SecretEngine) GetID() string { return e.Name }

// GetCreateIndex returns the engine's create index. Used for pagination.
func (e *SecretEngine) GetCreateIndex() uint64 { return e.CreateIndex }

// Canonicalize sets the defaults of the roles.
func (e *SecretEngine) Canonicalize() {
	for _, r := range e.Roles {
		if r == nil {
			continue
		}
		if r.TTL == 0 {
			r.TTL = SecretLeaseDefaultTTL
		}
		if r.MaxTTL == 0 {
			r.MaxTTL = max(SecretLeaseDefaultMaxTTL, r.TTL)
		}
	}
}

// Validate checks the engine as written by a user.
func (e *SecretEngine) Validate() error {
	var mErr *multierror.Error
	add := func(format string, args ...any) {
		mErr = multierror.Append(mErr, fmt.Errorf(format, args...))
	}
	if err := ValidateSecretEngineName("engine", e.Name); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	if len(e.Description) > 1024 {
		add("description is limited to 1024 bytes")
	}
	switch e.Type {
	case SecretEngineTypeOperator:
		if e.Operator == nil || e.Operator.Service == "" {
			add("an operator engine requires an operator block with a service")
		}
	case "":
		add("an engine requires a type")
	default:
		add("unsupported engine type %q (supported: %s)", e.Type, SecretEngineTypeOperator)
	}

	if len(e.Roles) == 0 {
		add("an engine requires at least one role")
	}
	if len(e.Roles) > 256 {
		add("an engine can have at most 256 roles")
	}
	names := map[string]struct{}{}
	for _, r := range e.Roles {
		if r == nil {
			continue
		}
		if _, ok := names[r.Name]; ok {
			add("duplicate role %q", r.Name)
		}
		names[r.Name] = struct{}{}
		if err := r.validate(e.Type); err != nil {
			mErr = multierror.Append(mErr, multierror.Prefix(err, fmt.Sprintf("role %q:", r.Name)))
		}
	}
	return mErr.ErrorOrNil()
}

// Copy returns a deep copy of the role.
func (r *SecretEngineRole) Copy() *SecretEngineRole {
	if r == nil {
		return nil
	}
	out := *r
	out.Config = maps.Clone(r.Config)
	out.Params = slices.Clone(r.Params)
	return &out
}

func (r *SecretEngineRole) validate(engineType string) error {
	var mErr *multierror.Error
	add := func(format string, args ...any) {
		mErr = multierror.Append(mErr, fmt.Errorf(format, args...))
	}
	if err := ValidateSecretEngineName("role", r.Name); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	if r.TTL < SecretLeaseMinTTL {
		add("ttl must be at least %s", SecretLeaseMinTTL)
	}
	if r.MaxTTL < r.TTL {
		add("max_ttl must not be shorter than ttl")
	}
	return mErr.ErrorOrNil()
}

// SecretEngineStub is the list form of an engine.
type SecretEngineStub struct {
	Namespace   string
	Name        string
	Type        string
	Description string
	Roles       []string
	CreateIndex uint64
	ModifyIndex uint64
	CreateTime  int64
	ModifyTime  int64
}

// SecretLease is the lease of a credential created by an engine. It never
// contains the credential.
type SecretLease struct {
	ID        string
	Namespace string
	Engine    string

	// EngineType is the type of the engine at the creation.
	EngineType string
	Role       string

	// SecretName is the label of the task's secret block.
	SecretName string

	// The task the credential was created for.
	AllocID   string
	JobID     string
	TaskGroup string
	Task      string
	NodeID    string

	// TTL and MaxTTL are the role's durations at the creation.
	TTL    time.Duration
	MaxTTL time.Duration

	// IssueTime is when the credential was created, ExpireTime when the
	// lease ends unless renewed, MaxExpireTime the end of max_ttl, at which
	// the credential itself expires (all Unix nanoseconds).
	IssueTime     int64
	ExpireTime    int64
	MaxExpireTime int64

	// RenewTime is the time of the last renewal, Renewals their number.
	RenewTime int64 `json:",omitempty"`
	Renewals  int   `json:",omitempty"`

	// Revoked is set once the lease is revoked or expired; RevokeTime and
	// RevokeReason say when and why. A revoked lease is kept until
	// MaxExpireTime, as the backend's revocation list refers to it.
	Revoked      bool
	RevokeTime   int64  `json:",omitempty"`
	RevokeReason string `json:",omitempty"`

	// BackendRef identifies the credential at the operator. It is not
	// secret.
	BackendRef string

	CreateIndex uint64
	ModifyIndex uint64
}

// Copy returns a copy of the lease.
func (l *SecretLease) Copy() *SecretLease {
	if l == nil {
		return nil
	}
	out := *l
	return &out
}

// CredsPath returns the virtual secret path of the lease's credential.
func (l *SecretLease) CredsPath() string {
	return SecretEngineCredsPath(l.Engine, l.Role)
}

// Live reports whether the lease is neither revoked nor expired at now.
func (l *SecretLease) Live(now time.Time) bool {
	return !l.Revoked && now.UnixNano() < l.ExpireTime
}

// GetNamespace returns the lease's namespace. Used for pagination.
func (l *SecretLease) GetNamespace() string { return l.Namespace }

// GetID returns the lease's ID. Used for pagination.
func (l *SecretLease) GetID() string { return l.ID }

// GetCreateIndex returns the lease's create index. Used for pagination.
func (l *SecretLease) GetCreateIndex() uint64 { return l.CreateIndex }

// SecretEngineEvent is the payload of the engine events of the Secret topic.
type SecretEngineEvent struct {
	Engine *SecretEngine
}

// SecretLeaseEvent is the payload of the lease events of the Secret topic.
// It never contains the credential.
type SecretLeaseEvent struct {
	Lease *SecretLease
}

// ---------------------------------------
// RPC and FSM request/response objects

// SecretEngineListRequest lists the engines of the request namespace (or of
// all namespaces with "*") whose name starts with the Prefix of the query
// options.
type SecretEngineListRequest struct {
	QueryOptions
}

// SecretEngineListResponse is the list of engine stubs.
type SecretEngineListResponse struct {
	Engines []*SecretEngineStub
	QueryMeta
}

// SecretEngineReadRequest reads an engine.
type SecretEngineReadRequest struct {
	Name string
	QueryOptions
}

// SecretEngineResponse is an engine; nil if there is none.
type SecretEngineResponse struct {
	Engine *SecretEngine
	QueryMeta
}

// SecretEngineUpsertRequest creates or updates an engine.
type SecretEngineUpsertRequest struct {
	Engine *SecretEngine

	// CheckIndex, if set, makes the write conditional: it succeeds only if
	// the engine's ModifyIndex equals it, or if it is 0 and the engine does
	// not exist.
	CheckIndex *uint64

	WriteRequest
}

// SecretEngineDeleteRequest deletes an engine.
type SecretEngineDeleteRequest struct {
	Name       string
	CheckIndex *uint64
	WriteRequest
}

// SecretEngineWriteResponse is the engine after a write; nil after a
// delete.
type SecretEngineWriteResponse struct {
	Engine *SecretEngine
	WriteMeta
}

// SecretEngineApplyRequest is the Raft request of an engine write
// (SecretEngineApplyRequestType), created by the leader only.
type SecretEngineApplyRequest struct {
	Engine *SecretEngine

	// ExpectedIndex is the ModifyIndex of the engine the leader computed the
	// write from, 0 if there was none.
	ExpectedIndex uint64

	WriteRequest
}

// SecretEnginePurgeRequest is the Raft request that removes an engine and
// the leases of the engine (SecretEnginePurgeRequestType), created by the
// leader only.
type SecretEnginePurgeRequest struct {
	Namespace     string
	Name          string
	ExpectedIndex uint64
	WriteRequest
}

// SecretLeaseListRequest lists the leases of the request namespace (or of
// all namespaces with "*"), optionally of one engine.
type SecretLeaseListRequest struct {
	Engine string
	QueryOptions
}

// SecretLeaseListResponse is the list of leases.
type SecretLeaseListResponse struct {
	Leases []*SecretLease
	QueryMeta
}

// SecretLeaseReadRequest reads a lease.
type SecretLeaseReadRequest struct {
	LeaseID string
	QueryOptions
}

// SecretLeaseResponse is a lease; nil if there is none.
type SecretLeaseResponse struct {
	Lease *SecretLease
	QueryMeta
}

// SecretLeaseCreateRequest asks for a credential of a role of an engine for
// the task of the request's workload identity. SecretName is the label of
// the task's secret block that names the engine and the role.
type SecretLeaseCreateRequest struct {
	Engine     string
	Role       string
	SecretName string
	WriteRequest
}

// SecretLeaseCreateResponse is the created credential and its lease. Items
// is the credential: it must never be logged or stored.
type SecretLeaseCreateResponse struct {
	Lease *SecretLease
	Items SecretItems

	WriteMeta
}

// SecretLeaseRenewRequest extends a lease.
type SecretLeaseRenewRequest struct {
	LeaseID string
	WriteRequest
}

// SecretLeaseWriteResponse is the lease after a renewal.
type SecretLeaseWriteResponse struct {
	Lease *SecretLease

	// Items are the new values of a credential that its renewal replaced,
	// for example a certificate. They are not stored.
	Items SecretItems `json:",omitempty"`

	WriteMeta
}

// SecretLeaseRevokeRequest revokes the lease LeaseID, or every live lease
// of Engine.
type SecretLeaseRevokeRequest struct {
	LeaseID string
	Engine  string
	WriteRequest
}

// SecretLeaseRevokeResponse lists the revoked leases.
type SecretLeaseRevokeResponse struct {
	Revoked []string
	WriteMeta
}

// SecretLeaseApplyRequest is the Raft request that creates, renews or
// revokes leases (SecretLeaseApplyRequestType), created by the leader only.
type SecretLeaseApplyRequest struct {
	Leases []*SecretLease
	WriteRequest
}

// SecretLeasePurgeRequest is the Raft request that removes ended leases
// (SecretLeasePurgeRequestType), created by the leader only.
type SecretLeasePurgeRequest struct {
	IDs []string
	WriteRequest
}
