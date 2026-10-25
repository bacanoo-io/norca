// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
)

// Ecosystem of the operator framework (phase 4): type-bound admission
// webhooks, schema versions with migration CEL, and server-side load
// management of the objects of a kind. All of it lives in fields of the
// ObjectKind and the CustomObject; there are no Raft messages of its own.

const (
	// Modes of type-bound webhooks: "validate" webhooks accept or reject a
	// write, "default" webhooks may also change the spec with a JSON patch
	// (defaulting), before the schema and the CEL rules are checked.
	ObjectWebhookModeValidate = "validate"
	ObjectWebhookModeDefault  = "default"

	// Failure policies of type-bound webhooks. "fail" (the default) rejects
	// the write when the webhook can't be called, isn't approved or returns
	// an invalid response; "ignore" lets it through with a warning.
	ObjectWebhookFailurePolicyFail   = "fail"
	ObjectWebhookFailurePolicyIgnore = "ignore"

	// Operations type-bound webhooks can review. Defaulting webhooks review
	// create and update only.
	ObjectWebhookOperationCreate = "create"
	ObjectWebhookOperationUpdate = "update"
	ObjectWebhookOperationDelete = "delete"

	// DefaultObjectWebhookTimeout and MaxObjectWebhookTimeout bound a
	// webhook call.
	DefaultObjectWebhookTimeout = 5 * time.Second
	MaxObjectWebhookTimeout     = 30 * time.Second

	// ObjectMigrationFailedReason is the reason of the event written to the
	// log of an object whose migration failed; ObjectMigratedReason of one
	// migrated by the background migration.
	ObjectMigrationFailedReason = "MigrationFailed"
	ObjectMigratedReason        = "Migrated"

	// AdmissionResourceObject is the resource of admission reviews of
	// custom objects, sent to type-bound webhooks.
	AdmissionResourceObject = "object"

	// MaxObjectMigrationFailures and MaxObjectMigrationSamples bound the
	// details of a migration report.
	MaxObjectMigrationFailures = 20
	MaxObjectMigrationSamples  = 3

	maxObjectKindWebhooks            = 8
	maxObjectWebhookCACertLength     = 64 * 1024
	maxObjectWebhookURLLength        = 2048
	maxObjectMigrationExprLength     = 16 * 1024
	maxObjectLimitRate               = 1e6
	maxObjectLimitBurst              = 1e6
	maxObjectLimitObjects            = 1e7
	maxObjectLimitWatchesPerIdentity = 1e5
)

// validObjectWebhookName is the rule of type-bound webhook names.
var validObjectWebhookName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,62}[a-zA-Z0-9])?$`)

// ObjectKindWebhook is an admission webhook bound to one object kind. It
// receives only the writes of objects of its kind, so registering it is a
// matter of the kind (and of the package that ships it) and not of the
// server configuration like the general admission webhooks. A webhook is
// called only once an administrator approved it: kinds written with a
// management token approve their webhooks implicitly, others need an
// explicit approval ("norca object-kind webhook approve"), which any change
// of the webhook's configuration revokes.
type ObjectKindWebhook struct {
	// Name identifies the webhook within the kind.
	Name string

	// Mode is "validate" (default) or "default".
	Mode string

	// URL is the https URL the admission review is POSTed to.
	URL string

	// CACert is a PEM bundle of the CAs that sign the webhook's
	// certificate, provided by the package; the system roots are used when
	// it is empty.
	CACert string

	// TLSServerName overrides the server name verified in the certificate.
	TLSServerName string

	// Timeout of a call, 5s by default and at most 30s.
	Timeout time.Duration

	// FailurePolicy is "fail" (default) or "ignore".
	FailurePolicy string

	// Operations are the reviewed operations: "create", "update" and, for
	// validating webhooks, "delete". Empty means create and update.
	Operations []string

	// Approval is set by the servers when an administrator approved the
	// webhook. Writes of kinds ignore it.
	Approval *ObjectKindWebhookApproval
}

// ObjectKindWebhookApproval records the approval of a type-bound webhook.
// It is valid while Digest matches the webhook's configuration.
type ObjectKindWebhookApproval struct {
	Digest      string
	ApprovedBy  string
	ApproveTime int64
}

// ObjectKindMigration converts the stored objects of a kind to a new schema
// version. The expressions are CEL with the variables "self" (the stored
// object, like in the rules), "fromVersion" (its schema version) and
// "toVersion"; maps have merge() and omit() helpers.
type ObjectKindMigration struct {
	// Spec returns the new spec. Empty keeps the spec.
	Spec string

	// Status returns the new status. Empty keeps the status; an empty map
	// drops it (the controller rewrites it).
	Status string
}

// ObjectKindLimits is the server-side load management of the objects of a
// kind. Zero values mean no limit. Rates are writes per second (token
// buckets on the leader); a write over a limit fails with 429 Too Many
// Requests, which controllers retry with backoff.
type ObjectKindLimits struct {
	// WriteRate and WriteBurst limit all writes of objects of the kind
	// (spec, status, finalizers, events, scale, deletion).
	WriteRate  float64
	WriteBurst int

	// IdentityWriteRate and IdentityWriteBurst limit the writes of each
	// identity (token or workload), so that a misbehaving controller does
	// not use up the kind's budget of users.
	IdentityWriteRate  float64
	IdentityWriteBurst int

	// MaxObjectsPerNamespace limits the number of objects of the kind in a
	// namespace (for cluster-scoped kinds: in total).
	MaxObjectsPerNamespace int

	// MaxWatchesPerIdentity limits the concurrent blocking queries of each
	// identity on objects of the kind, per server.
	MaxWatchesPerIdentity int
}

// SchemaVersion returns the schema version of the kind (at least 1).
func (k *ObjectKind) SchemaVersion() uint64 {
	return max(1, k.Version)
}

// SchemaVersion returns the schema version of the kind the object was last
// written or migrated at (at least 1).
func (o *CustomObject) SchemaVersion() uint64 {
	return max(1, o.KindVersion)
}

// Canonicalize sets the defaults of the ecosystem fields of the kind.
func (k *ObjectKind) Canonicalize() {
	if k == nil {
		return
	}
	if k.Version == 0 {
		k.Version = 1
	}
	for _, w := range k.Webhooks {
		if w == nil {
			continue
		}
		if w.Mode == "" {
			w.Mode = ObjectWebhookModeValidate
		}
		if w.FailurePolicy == "" {
			w.FailurePolicy = ObjectWebhookFailurePolicyFail
		}
		if w.Timeout == 0 {
			w.Timeout = DefaultObjectWebhookTimeout
		}
		if len(w.Operations) == 0 {
			w.Operations = []string{ObjectWebhookOperationCreate, ObjectWebhookOperationUpdate}
		}
	}
	if l := k.Limits; l != nil {
		if l.WriteRate > 0 && l.WriteBurst == 0 {
			l.WriteBurst = int(math.Ceil(l.WriteRate))
		}
		if l.IdentityWriteRate > 0 && l.IdentityWriteBurst == 0 {
			l.IdentityWriteBurst = int(math.Ceil(l.IdentityWriteRate))
		}
	}
	if m := k.Migration; m != nil && strings.TrimSpace(m.Spec) == "" && strings.TrimSpace(m.Status) == "" {
		k.Migration = nil
	}
}

// validateEcosystem validates the webhooks, the migration and the limits of
// the kind. The migration expressions are compiled by the server.
func (k *ObjectKind) validateEcosystem() error {
	var mErr *multierror.Error
	if len(k.Webhooks) > maxObjectKindWebhooks {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d webhooks are allowed", maxObjectKindWebhooks))
	}
	seen := make(map[string]struct{}, len(k.Webhooks))
	for i, w := range k.Webhooks {
		if w == nil {
			mErr = multierror.Append(mErr, fmt.Errorf("webhook %d: empty", i))
			continue
		}
		if !validObjectWebhookName.MatchString(w.Name) {
			mErr = multierror.Append(mErr, fmt.Errorf("webhook %d: invalid name %q, must match regex %s", i, w.Name, validObjectWebhookName))
		}
		if _, ok := seen[w.Name]; ok {
			mErr = multierror.Append(mErr, fmt.Errorf("webhook %q: duplicate name", w.Name))
		}
		seen[w.Name] = struct{}{}
		if err := w.validate(); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("webhook %q: %v", w.Name, err))
		}
	}
	if m := k.Migration; m != nil {
		if len(m.Spec) > maxObjectMigrationExprLength || len(m.Status) > maxObjectMigrationExprLength {
			mErr = multierror.Append(mErr, fmt.Errorf("migration: expression longer than %d", maxObjectMigrationExprLength))
		}
	}
	if l := k.Limits; l != nil {
		if err := l.validate(); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("limits: %v", err))
		}
	}
	return mErr.ErrorOrNil()
}

func (w *ObjectKindWebhook) validate() error {
	var mErr *multierror.Error
	switch w.Mode {
	case "", ObjectWebhookModeValidate, ObjectWebhookModeDefault:
	default:
		mErr = multierror.Append(mErr, fmt.Errorf("invalid mode %q, must be %q or %q", w.Mode,
			ObjectWebhookModeValidate, ObjectWebhookModeDefault))
	}
	if len(w.URL) > maxObjectWebhookURLLength {
		mErr = multierror.Append(mErr, fmt.Errorf("url longer than %d", maxObjectWebhookURLLength))
	} else if u, err := url.Parse(w.URL); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		mErr = multierror.Append(mErr, fmt.Errorf("url must be an absolute https URL without user information"))
	}
	if len(w.CACert) > maxObjectWebhookCACertLength {
		mErr = multierror.Append(mErr, fmt.Errorf("ca_cert longer than %d", maxObjectWebhookCACertLength))
	} else if w.CACert != "" && !strings.Contains(w.CACert, "-----BEGIN CERTIFICATE-----") {
		mErr = multierror.Append(mErr, fmt.Errorf("ca_cert must be a PEM certificate bundle"))
	}
	if len(w.TLSServerName) > 253 {
		mErr = multierror.Append(mErr, fmt.Errorf("tls_server_name longer than 253"))
	}
	if w.Timeout < 0 || w.Timeout > MaxObjectWebhookTimeout {
		mErr = multierror.Append(mErr, fmt.Errorf("timeout must be between 0 and %s", MaxObjectWebhookTimeout))
	}
	switch w.FailurePolicy {
	case "", ObjectWebhookFailurePolicyFail, ObjectWebhookFailurePolicyIgnore:
	default:
		mErr = multierror.Append(mErr, fmt.Errorf("invalid failure_policy %q, must be %q or %q", w.FailurePolicy,
			ObjectWebhookFailurePolicyFail, ObjectWebhookFailurePolicyIgnore))
	}
	for _, op := range w.Operations {
		switch op {
		case ObjectWebhookOperationCreate, ObjectWebhookOperationUpdate:
		case ObjectWebhookOperationDelete:
			if w.Mode == ObjectWebhookModeDefault {
				mErr = multierror.Append(mErr, fmt.Errorf("defaulting webhooks can't review %q", op))
			}
		default:
			mErr = multierror.Append(mErr, fmt.Errorf("invalid operation %q, must be %q, %q or %q", op,
				ObjectWebhookOperationCreate, ObjectWebhookOperationUpdate, ObjectWebhookOperationDelete))
		}
	}
	return mErr.ErrorOrNil()
}

func (l *ObjectKindLimits) validate() error {
	var mErr *multierror.Error
	check := func(name string, v, limit float64) {
		if v < 0 || v > limit || math.IsNaN(v) {
			mErr = multierror.Append(mErr, fmt.Errorf("%s must be between 0 and %g", name, limit))
		}
	}
	check("write_rate", l.WriteRate, maxObjectLimitRate)
	check("write_burst", float64(l.WriteBurst), maxObjectLimitBurst)
	check("identity_write_rate", l.IdentityWriteRate, maxObjectLimitRate)
	check("identity_write_burst", float64(l.IdentityWriteBurst), maxObjectLimitBurst)
	check("max_objects_per_namespace", float64(l.MaxObjectsPerNamespace), maxObjectLimitObjects)
	check("max_watches_per_identity", float64(l.MaxWatchesPerIdentity), maxObjectLimitWatchesPerIdentity)
	return mErr.ErrorOrNil()
}

// copyEcosystem deep-copies the ecosystem fields of k into nk.
func (k *ObjectKind) copyEcosystem(nk *ObjectKind) {
	if k.Webhooks != nil {
		nk.Webhooks = make([]*ObjectKindWebhook, len(k.Webhooks))
		for i, w := range k.Webhooks {
			nk.Webhooks[i] = w.Copy()
		}
	}
	if k.Migration != nil {
		m := *k.Migration
		nk.Migration = &m
	}
	if k.Limits != nil {
		l := *k.Limits
		nk.Limits = &l
	}
}

// Copy returns a deep copy of the webhook.
func (w *ObjectKindWebhook) Copy() *ObjectKindWebhook {
	if w == nil {
		return nil
	}
	nw := *w
	nw.Operations = slices.Clone(w.Operations)
	if w.Approval != nil {
		a := *w.Approval
		nw.Approval = &a
	}
	return &nw
}

// Digest returns the hash of the webhook's configuration that an approval
// covers: everything but the approval itself.
func (w *ObjectKindWebhook) Digest() string {
	c := w.Copy()
	c.Approval = nil
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Approved reports whether the webhook has a valid approval.
func (w *ObjectKindWebhook) Approved() bool {
	return w.Approval != nil && w.Approval.Digest == w.Digest()
}

// Reviews reports whether the webhook reviews the operation.
func (w *ObjectKindWebhook) Reviews(operation string) bool {
	ops := w.Operations
	if len(ops) == 0 {
		ops = []string{ObjectWebhookOperationCreate, ObjectWebhookOperationUpdate}
	}
	return slices.Contains(ops, operation)
}

// IgnoreFailure reports whether a failed call lets the write through.
func (w *ObjectKindWebhook) IgnoreFailure() bool {
	return w.FailurePolicy == ObjectWebhookFailurePolicyIgnore
}

// Webhook returns the webhook of the kind with the name, or nil.
func (k *ObjectKind) Webhook(name string) *ObjectKindWebhook {
	for _, w := range k.Webhooks {
		if w != nil && w.Name == name {
			return w
		}
	}
	return nil
}

// WithoutApprovals returns a copy of the kind without the approvals of its
// webhooks, for comparing the configuration of kinds.
func (k *ObjectKind) WithoutApprovals() *ObjectKind {
	c := k.Copy()
	for _, w := range c.Webhooks {
		if w != nil {
			w.Approval = nil
		}
	}
	return c
}

// ObjectKindWebhookApprovalRequest approves (or revokes the approval of) a
// type-bound webhook. It requires a management token.
type ObjectKindWebhookApprovalRequest struct {
	Group   string
	Kind    string
	Webhook string

	// Revoke removes the approval instead.
	Revoke bool

	WriteRequest
}

// ObjectKindMigrationReport describes what a kind write does to the stored
// objects of the kind: how many are converted by the migration, and which
// would no longer be valid under the new schema.
type ObjectKindMigrationReport struct {
	// FromVersion and ToVersion are the stored and the new schema version.
	FromVersion uint64
	ToVersion   uint64

	// Objects is the number of stored objects checked; Pending the number
	// of them at an older schema version than ToVersion, which the
	// migration converts; Changed the number whose spec or status the
	// migration changes.
	Objects int
	Pending int
	Changed int

	// Failed is the number of objects that would be invalid; Failures
	// details the first of them.
	Failed   int
	Failures []*ObjectMigrationFailure

	// Samples show the change of the first changed objects.
	Samples []*ObjectMigrationSample
}

// ObjectMigrationFailure is an object a kind write would make invalid.
type ObjectMigrationFailure struct {
	Namespace string
	Name      string
	Error     string
}

// ObjectMigrationSample shows how the migration changes an object.
type ObjectMigrationSample struct {
	Namespace    string
	Name         string
	SpecBefore   map[string]any
	SpecAfter    map[string]any
	StatusBefore map[string]any
	StatusAfter  map[string]any
}

// NewObjectLimitError returns the 429 error of a request over a limit of
// the kind.
func NewObjectLimitError(format string, args ...any) error {
	return NewErrRPCCoded(429, "object limit exceeded: "+fmt.Sprintf(format, args...))
}
