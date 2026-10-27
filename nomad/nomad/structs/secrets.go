// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Secrets of the built-in secret store (norca-doc content/docs/design/secrets-plan.md, "The
// built-in store"). The values and their version history live in the
// uBixVault KV v2 engine on the barrier of the secret store: they are only
// ever stored encrypted, as SecretStoreBlob entries. A Secret is the
// metadata row of one secret path in the secrets table: everything about the
// secret except its values, so it can be listed, watched with blocking
// queries and published on the event stream. It carries a keyed hash of the
// values (HMAC with a key kept in the barrier), so that a value change can be
// detected without the value and values cannot be guessed offline from the
// hash.

const (
	// SecretListRPCMethod lists the metadata of the secrets of a namespace.
	//
	// Args: SecretListRequest
	// Reply: SecretListResponse
	SecretListRPCMethod = "Secret.List"

	// SecretReadMetadataRPCMethod reads the metadata of a secret, never its
	// value.
	//
	// Args: SecretReadMetadataRequest
	// Reply: SecretMetadataResponse
	SecretReadMetadataRPCMethod = "Secret.ReadMetadata"

	// SecretReadRPCMethod reads the value of a version of a secret. It is
	// always executed by the leader, and audited.
	//
	// Args: SecretReadRequest
	// Reply: SecretReadResponse
	SecretReadRPCMethod = "Secret.Read"

	// SecretUpsertRPCMethod writes a new version of a secret and/or its
	// metadata.
	//
	// Args: SecretUpsertRequest
	// Reply: SecretWriteResponse
	SecretUpsertRPCMethod = "Secret.Upsert"

	// SecretDeleteRPCMethod soft deletes versions of a secret.
	//
	// Args: SecretVersionsRequest
	// Reply: SecretWriteResponse
	SecretDeleteRPCMethod = "Secret.Delete"

	// SecretUndeleteRPCMethod restores soft deleted versions of a secret.
	//
	// Args: SecretVersionsRequest
	// Reply: SecretWriteResponse
	SecretUndeleteRPCMethod = "Secret.Undelete"

	// SecretDestroyRPCMethod destroys versions of a secret permanently, or
	// the whole secret when no version is given.
	//
	// Args: SecretVersionsRequest
	// Reply: SecretWriteResponse
	SecretDestroyRPCMethod = "Secret.Destroy"

	// SecretRecordAccessRPCMethod records the audit record of a secret
	// fetch a client made from an external provider for a task. It is
	// authenticated with the task's workload identity, which becomes the
	// accessor of the record.
	//
	// Args: SecretRecordAccessRequest
	// Reply: GenericResponse
	SecretRecordAccessRPCMethod = "Secret.RecordAccess"
)

const (
	// MaxSecretSize is the maximum total size of the keys and values of the
	// items of one secret version.
	MaxSecretSize = 65536

	// SecretDefaultMaxVersions is the number of versions kept per secret
	// unless the secret sets MaxVersions; older versions are destroyed.
	SecretDefaultMaxVersions = 10

	// SecretMaxMaxVersions bounds MaxVersions, so that the version history
	// of one secret stays within a small Raft entry.
	SecretMaxMaxVersions = 100

	// SecretValueHashPrefix prefixes the keyed value hashes.
	SecretValueHashPrefix = "hmac-sha256:"
)

const (
	// TopicSecret carries the changes of secrets: metadata and value hash,
	// never values.
	TopicSecret Topic = "Secret"

	// TopicSecretAccess carries the audit records of the secret store: every
	// value read and every write, with the accessor and the result.
	TopicSecretAccess Topic = "SecretAccess"

	TypeSecretUpserted = "SecretUpserted"
	TypeSecretDeleted  = "SecretDeleted"
	TypeSecretAccessed = "SecretAccessed"
)

var (
	// ErrSecretNotFound is returned for a secret path without a secret.
	ErrSecretNotFound = errors.New("secret not found")

	// ErrSecretConflict is returned when the check index of a write does not
	// match the secret, or when the secret was modified concurrently.
	ErrSecretConflict = errors.New("secret was modified concurrently (check index mismatch)")

	// validSecretPath restricts secret paths to URL safe characters that
	// need no escaping in templates; "." is excluded, so no path segment can
	// be "." or "..".
	validSecretPath = regexp.MustCompile("^[a-zA-Z0-9-_~/]{1,256}$")

	// validSecretItemKey restricts item keys to names that can be used as
	// file names and environment variable names.
	validSecretItemKey = regexp.MustCompile("^[a-zA-Z0-9_.-]{1,128}$")
)

// SecretItems are the values of one secret version: key/value pairs.
type SecretItems map[string]string

// Size returns the total size of the keys and values.
func (si SecretItems) Size() int {
	n := 0
	for k, v := range si {
		n += len(k) + len(v)
	}
	return n
}

// Keys returns the sorted item keys.
func (si SecretItems) Keys() []string {
	keys := make([]string, 0, len(si))
	for k := range si {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Copy returns a copy of the items.
func (si SecretItems) Copy() SecretItems {
	if si == nil {
		return nil
	}
	out := make(SecretItems, len(si))
	for k, v := range si {
		out[k] = v
	}
	return out
}

// Validate checks the items of a version.
func (si SecretItems) Validate() error {
	if len(si) == 0 {
		return errors.New("a secret needs at least one item")
	}
	if si.Size() > MaxSecretSize {
		return fmt.Errorf("secrets are limited to %d bytes in total size", MaxSecretSize)
	}
	for k := range si {
		if !validSecretItemKey.MatchString(k) {
			return fmt.Errorf("invalid item key %q", k)
		}
	}
	return nil
}

// SecretMeta is the user settable metadata of a secret. It is never secret:
// everyone who may list a secret sees it.
type SecretMeta struct {
	// Description is free text.
	Description string

	// Owner names the person or team responsible for the secret.
	Owner string

	// Labels are free key/value labels.
	Labels map[string]string

	// ExpiresAt is when the secret's value expires, if set. Expiry warnings
	// and events are produced by a later phase; the store keeps and shows it.
	ExpiresAt *time.Time

	// RotateEvery is the interval the value should be rotated in, if set.
	RotateEvery time.Duration

	// MaxVersions is the number of versions kept; older versions are
	// destroyed on the next write. 0 means SecretDefaultMaxVersions.
	MaxVersions int
}

// Copy returns a deep copy of the metadata.
func (m *SecretMeta) Copy() *SecretMeta {
	if m == nil {
		return nil
	}
	out := *m
	if m.Labels != nil {
		out.Labels = make(map[string]string, len(m.Labels))
		for k, v := range m.Labels {
			out.Labels[k] = v
		}
	}
	if m.ExpiresAt != nil {
		t := *m.ExpiresAt
		out.ExpiresAt = &t
	}
	return &out
}

// Canonicalize sets the defaults.
func (m *SecretMeta) Canonicalize() {
	if m.MaxVersions == 0 {
		m.MaxVersions = SecretDefaultMaxVersions
	}
}

// Validate checks the metadata.
func (m *SecretMeta) Validate() error {
	if m.MaxVersions < 0 || m.MaxVersions > SecretMaxMaxVersions {
		return fmt.Errorf("max_versions must be between 1 and %d", SecretMaxMaxVersions)
	}
	if m.RotateEvery < 0 {
		return errors.New("rotate_every must not be negative")
	}
	if len(m.Description) > 1024 {
		return errors.New("description is limited to 1024 bytes")
	}
	if len(m.Owner) > 256 {
		return errors.New("owner is limited to 256 bytes")
	}
	if len(m.Labels) > 64 {
		return errors.New("a secret can have at most 64 labels")
	}
	for k, v := range m.Labels {
		if k == "" || len(k) > 128 || len(v) > 512 {
			return fmt.Errorf("invalid label %q", k)
		}
	}
	return nil
}

// SecretVersion describes one retained version of a secret, never its value.
type SecretVersion struct {
	Version    uint64
	CreateTime int64

	// Deleted is set for a soft deleted version, which can be undeleted.
	Deleted bool

	// Destroyed is set for a version whose value was destroyed permanently.
	Destroyed bool

	// ValueHash is the keyed hash of the version's items; empty once the
	// version is destroyed.
	ValueHash string
}

// Secret is the metadata row of a secret in the secrets table. It never
// contains values.
type Secret struct {
	Namespace string
	Path      string

	SecretMeta

	// CurrentVersion is the latest version.
	CurrentVersion uint64

	// Deleted and Destroyed describe the current version.
	Deleted   bool
	Destroyed bool

	// ItemKeys are the sorted item keys of the current version.
	ItemKeys []string

	// ValueHash is the keyed hash of the current version's items.
	ValueHash string

	// Versions are the retained versions, oldest first.
	Versions []*SecretVersion

	CreateIndex uint64
	ModifyIndex uint64
	CreateTime  int64
	ModifyTime  int64
}

// Copy returns a deep copy of the secret.
func (s *Secret) Copy() *Secret {
	if s == nil {
		return nil
	}
	out := *s
	if m := s.SecretMeta.Copy(); m != nil {
		out.SecretMeta = *m
	}
	out.ItemKeys = append([]string(nil), s.ItemKeys...)
	if s.Versions != nil {
		out.Versions = make([]*SecretVersion, len(s.Versions))
		for i, v := range s.Versions {
			vv := *v
			out.Versions[i] = &vv
		}
	}
	return &out
}

// Version returns the retained version, or nil.
func (s *Secret) Version(v uint64) *SecretVersion {
	for _, sv := range s.Versions {
		if sv.Version == v {
			return sv
		}
	}
	return nil
}

// Stub returns the list form of the secret.
func (s *Secret) Stub() *SecretStub {
	m := s.SecretMeta.Copy()
	return &SecretStub{
		Namespace:      s.Namespace,
		Path:           s.Path,
		Description:    m.Description,
		Owner:          m.Owner,
		Labels:         m.Labels,
		ExpiresAt:      m.ExpiresAt,
		RotateEvery:    m.RotateEvery,
		CurrentVersion: s.CurrentVersion,
		Deleted:        s.Deleted,
		Destroyed:      s.Destroyed,
		ValueHash:      s.ValueHash,
		CreateIndex:    s.CreateIndex,
		ModifyIndex:    s.ModifyIndex,
		CreateTime:     s.CreateTime,
		ModifyTime:     s.ModifyTime,
	}
}

// GetNamespace returns the secret's namespace. Used for pagination.
func (s *Secret) GetNamespace() string { return s.Namespace }

// GetID returns the secret's path. Used for pagination.
func (s *Secret) GetID() string { return s.Path }

// GetCreateIndex returns the secret's create index. Used for pagination.
func (s *Secret) GetCreateIndex() uint64 { return s.CreateIndex }

// SecretStub is the list form of a secret.
type SecretStub struct {
	Namespace      string
	Path           string
	Description    string
	Owner          string
	Labels         map[string]string
	ExpiresAt      *time.Time
	RotateEvery    time.Duration
	CurrentVersion uint64
	Deleted        bool
	Destroyed      bool
	ValueHash      string
	CreateIndex    uint64
	ModifyIndex    uint64
	CreateTime     int64
	ModifyTime     int64
}

// ValidateSecretPath checks a secret path: URL safe characters, no empty
// segments and no leading or trailing "/".
func ValidateSecretPath(path string) error {
	if path == "" {
		return errors.New("secret requires a path")
	}
	if !validSecretPath.MatchString(path) ||
		strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.Contains(path, "//") {
		return fmt.Errorf("invalid secret path %q", path)
	}
	return nil
}

// SecretEvent is the payload of the Secret topic: the metadata of the
// changed secret, never its values.
type SecretEvent struct {
	Secret *Secret
}

// SecretAccessor identifies who accessed a secret: an ACL token or a
// workload identity.
type SecretAccessor struct {
	// AccessorID is the accessor ID of the ACL token, if one was used.
	AccessorID string `json:",omitempty"`

	// The workload identity, if one was used.
	Namespace    string `json:",omitempty"`
	JobID        string `json:",omitempty"`
	TaskGroup    string `json:",omitempty"`
	Task         string `json:",omitempty"`
	AllocationID string `json:",omitempty"`

	// NodeID is the client node of the workload identity's allocation, or
	// of a client authenticated with its node secret.
	NodeID string `json:",omitempty"`

	// RemoteIP is the address the request came from, if known.
	RemoteIP string `json:",omitempty"`
}

// Secret access operations, as recorded in audit records.
const (
	SecretOpRead     = "read"
	SecretOpWrite    = "write"
	SecretOpDelete   = "delete"
	SecretOpUndelete = "undelete"
	SecretOpDestroy  = "destroy"
	SecretOpRestore  = "restore"
)

// Secret access results, as recorded in audit records.
const (
	SecretAccessResultOK       = "ok"
	SecretAccessResultDenied   = "denied"
	SecretAccessResultNotFound = "not-found"
	SecretAccessResultConflict = "conflict"
	SecretAccessResultSealed   = "unavailable"
	SecretAccessResultInvalid  = "invalid"
	SecretAccessResultError    = "error"
)

// SecretAccessEvent is an audit record of the secret store, the payload of
// the SecretAccess topic and a line of the audit file. It never contains a
// value.
type SecretAccessEvent struct {
	Time      time.Time
	Region    string
	Namespace string
	Path      string

	// Version is the version read or written; 0 if none applies.
	Version uint64

	// Versions are the versions a delete, undelete or destroy targeted.
	Versions []uint64 `json:",omitempty"`

	// BackupID and BackupVersion name the backup and the version of the
	// secret in it whose value a restore wrote as the new Version.
	BackupID      string `json:",omitempty"`
	BackupVersion uint64 `json:",omitempty"`

	Operation string
	Result    string

	// Error is the error message of a failed access.
	Error string `json:",omitempty"`

	Accessor *SecretAccessor

	// Server is the name of the server that executed the access.
	Server string

	// Provider is the external provider a client fetched the secret from
	// for a task; empty for accesses of the built-in store.
	Provider string `json:",omitempty"`

	// ProviderVersion is the version the external provider reported.
	ProviderVersion string `json:",omitempty"`
}

// SecretRecordAccessRequest is the audit record of a fetch from an external
// provider, sent by the client of the task. Only Path, Provider,
// ProviderVersion, Result and Error of the record are taken from the client;
// the accessor is the request's workload identity.
type SecretRecordAccessRequest struct {
	Record *SecretAccessEvent
	WriteRequest
}

// ---------------------------------------
// RPC and FSM request/response objects

// SecretListRequest lists the secrets of the request namespace (or of all
// namespaces with "*") whose path starts with the Prefix of the query
// options.
type SecretListRequest struct {
	QueryOptions
}

// SecretListResponse is the list of secret stubs.
type SecretListResponse struct {
	Secrets []*SecretStub
	QueryMeta
}

// SecretReadMetadataRequest reads the metadata of a secret.
type SecretReadMetadataRequest struct {
	Path string
	QueryOptions
}

// SecretMetadataResponse is the metadata of a secret; Secret is nil if
// there is none.
type SecretMetadataResponse struct {
	Secret *Secret
	QueryMeta
}

// SecretReadRequest reads the value of a version of a secret; Version 0
// reads the current version.
type SecretReadRequest struct {
	Path    string
	Version uint64
	QueryOptions
}

// SecretReadResponse is the value of a version of a secret and the secret's
// metadata. Secret is nil if there is no secret at the path.
type SecretReadResponse struct {
	Secret  *Secret
	Version uint64
	Items   SecretItems
	QueryMeta
}

// SecretUpsertRequest writes a secret.
type SecretUpsertRequest struct {
	Path string

	// Items is the value of the new version. If it is empty, only the
	// metadata of an existing secret is updated and no version is written.
	Items SecretItems

	// Meta replaces the user metadata of the secret. If it is nil, an
	// existing secret keeps its metadata and a new one gets the defaults.
	Meta *SecretMeta

	// CheckIndex, if set, makes the write conditional: it succeeds only if
	// the secret's ModifyIndex equals it, or if it is 0 and the secret does
	// not exist.
	CheckIndex *uint64

	WriteRequest
}

// SecretVersionsRequest deletes, undeletes or destroys versions of a
// secret. Without versions, delete and undelete act on the current version
// and destroy removes the whole secret with all of its versions.
type SecretVersionsRequest struct {
	Path     string
	Versions []uint64

	// CheckIndex, if set, makes the operation conditional on the secret's
	// ModifyIndex.
	CheckIndex *uint64

	WriteRequest
}

// SecretWriteResponse is the metadata of the secret after a write; Secret
// is nil after the whole secret was destroyed.
type SecretWriteResponse struct {
	Secret *Secret
	WriteMeta
}

// SecretApplyRequest is the Raft request of a secret write
// (SecretApplyRequestType): the new metadata row and the barrier blobs of
// the uBixVault KV engine that the write produced, applied in one
// transaction. It is created by the leader only.
type SecretApplyRequest struct {
	Secret *Secret

	// ExpectedIndex is the ModifyIndex of the row the leader computed the
	// write from, 0 if there was none. The apply fails with a conflict if
	// the row changed since.
	ExpectedIndex uint64

	// Blobs are the barrier blobs written, DeleteBlobs the paths of the
	// blobs removed.
	Blobs       []*SecretStoreBlob
	DeleteBlobs []string

	// CreateBlobs are paths among Blobs that must not exist yet (the value
	// hash key); the apply fails with a conflict if one does.
	CreateBlobs []string

	WriteRequest
}

// SecretPurgeRequest is the Raft request that removes a secret with all of
// its versions (SecretPurgeRequestType). It is created by the leader only.
type SecretPurgeRequest struct {
	Namespace     string
	Path          string
	ExpectedIndex uint64
	DeleteBlobs   []string
	WriteRequest
}
