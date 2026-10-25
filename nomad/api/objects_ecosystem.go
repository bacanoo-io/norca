// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Ecosystem of the operator framework: type-bound webhooks, schema versions
// with migration CEL, and load management of the objects of a kind.

const (
	// Modes of type-bound webhooks.
	ObjectWebhookModeValidate = "validate"
	ObjectWebhookModeDefault  = "default"

	// Failure policies of type-bound webhooks.
	ObjectWebhookFailurePolicyFail   = "fail"
	ObjectWebhookFailurePolicyIgnore = "ignore"

	// Operations type-bound webhooks review.
	ObjectWebhookOperationCreate = "create"
	ObjectWebhookOperationUpdate = "update"
	ObjectWebhookOperationDelete = "delete"
)

// ObjectKindWebhook is an admission webhook bound to an object kind. It is
// called only after an administrator approved it (kinds written with a
// management token approve their webhooks implicitly).
type ObjectKindWebhook struct {
	Name string

	// Mode is "validate" (default) or "default" (may patch the spec).
	Mode string

	// URL is the https URL of the webhook.
	URL string

	// CACert is the PEM CA bundle of the webhook's certificate.
	CACert string

	TLSServerName string

	// Timeout of a call (default 5s, at most 30s).
	Timeout time.Duration

	// FailurePolicy is "fail" (default) or "ignore".
	FailurePolicy string

	// Operations reviewed: "create", "update" (the default) and, for
	// validating webhooks, "delete".
	Operations []string

	// Approval is set by the servers; it is ignored on writes.
	Approval *ObjectKindWebhookApproval
}

// ObjectKindWebhookApproval records the approval of a webhook.
type ObjectKindWebhookApproval struct {
	Digest      string
	ApprovedBy  string
	ApproveTime int64
}

// ObjectKindMigration converts objects stored at older schema versions
// with CEL expressions (self, fromVersion, toVersion) that return the new
// spec and status.
type ObjectKindMigration struct {
	Spec   string
	Status string
}

// ObjectKindLimits is the load management of the objects of a kind; zero
// values mean no limit. Writes over a limit fail with 429 Too Many
// Requests (see IsObjectLimitError).
type ObjectKindLimits struct {
	WriteRate              float64
	WriteBurst             int
	IdentityWriteRate      float64
	IdentityWriteBurst     int
	MaxObjectsPerNamespace int
	MaxWatchesPerIdentity  int
}

// ObjectKindMigrationReport describes what a kind write does to the stored
// objects of the kind.
type ObjectKindMigrationReport struct {
	FromVersion uint64
	ToVersion   uint64
	Objects     int
	Pending     int
	Changed     int
	Failed      int
	Failures    []*ObjectMigrationFailure
	Samples     []*ObjectMigrationSample
}

// ObjectMigrationFailure is an object a kind write would make invalid.
type ObjectMigrationFailure struct {
	Namespace string
	Name      string
	Error     string
}

// ObjectMigrationSample shows how a migration changes an object.
type ObjectMigrationSample struct {
	Namespace    string
	Name         string
	SpecBefore   map[string]any
	SpecAfter    map[string]any
	StatusBefore map[string]any
	StatusAfter  map[string]any
}

// ObjectKindPlan is the result of a dry run of a kind write: the kind as it
// would be stored, the effect on the stored objects and warnings.
type ObjectKindPlan struct {
	ObjectKind *ObjectKind
	Migration  *ObjectKindMigrationReport
	Warnings   []string
}

// Approved reports whether the webhook has an approval of its current
// configuration. The servers decide; this only reports whether an approval
// is recorded.
func (w *ObjectKindWebhook) Approved() bool {
	return w != nil && w.Approval != nil
}

// SchemaVersion returns the schema version of the kind (at least 1).
func (k *ObjectKind) SchemaVersion() uint64 {
	return max(1, k.Version)
}

// IsObjectLimitError reports whether err is a 429 Too Many Requests
// response, returned when a write or watch exceeds a limit of the kind.
func IsObjectLimitError(err error) bool {
	var respErr UnexpectedResponseError
	return errors.As(err, &respErr) && respErr.StatusCode() == http.StatusTooManyRequests
}

// Plan runs a dry run of a kind write and returns the kind as it would be
// stored with the report of the stored objects (migration and validation
// against the new schema).
func (k *ObjectKinds) Plan(kind *ObjectKind, w *WriteOptions) (*ObjectKindPlan, *WriteMeta, error) {
	if kind == nil || kind.Group == "" || kind.Kind == "" {
		return nil, nil, errors.New("missing object kind group or name")
	}
	var resp ObjectKindPlan
	endpoint := "/v1/object-kind/" + url.PathEscape(kind.Group) + "/" + url.PathEscape(kind.Kind) + "/plan"
	wm, err := k.client.put(endpoint, kind, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// ApproveWebhook approves a type-bound webhook of a kind (a management
// token is required); revoke removes the approval. It returns the kind as
// stored.
func (k *ObjectKinds) ApproveWebhook(group, kind, webhook string, revoke bool, w *WriteOptions) (*ObjectKind, *WriteMeta, error) {
	if group == "" || kind == "" || webhook == "" {
		return nil, nil, errors.New("missing object kind group, name or webhook")
	}
	action := "approve"
	if revoke {
		action = "revoke"
	}
	var resp ObjectKind
	endpoint := "/v1/object-kind/" + url.PathEscape(group) + "/" + url.PathEscape(kind) +
		"/webhook/" + url.PathEscape(webhook) + "/" + action
	wm, err := k.client.put(endpoint, nil, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}
