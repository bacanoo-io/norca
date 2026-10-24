// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Pack releases: a Release records an installation of a pack with its
// revisions; the server applies the rendered resources of a pack in a fixed
// order, optionally atomically, with hooks and tests.

const (
	// ReleaseObjectGroup and ReleaseKind identify release objects in the
	// object API and in ACL rules (object "packs.norca.io/Release").
	ReleaseObjectGroup  = "packs.norca.io"
	ReleaseKind         = "Release"
	ReleaseRevisionKind = "ReleaseRevision"

	// Resource types of releases, in application order.
	ReleaseResourceNamespace       = "namespace"
	ReleaseResourceQuota           = "quota"
	ReleaseResourceACLPolicy       = "acl-policy"
	ReleaseResourceVariable        = "variable"
	ReleaseResourceObjectKind      = "object-kind"
	ReleaseResourceAdmissionPolicy = "admission-policy"
	ReleaseResourceObject          = "object"
	ReleaseResourceJob             = "job"

	// Release phases.
	ReleasePhasePending      = "pending"
	ReleasePhaseApplying     = "applying"
	ReleasePhaseDeployed     = "deployed"
	ReleasePhaseFailed       = "failed"
	ReleasePhaseRollingBack  = "rolling-back"
	ReleasePhaseSuperseded   = "superseded"
	ReleasePhaseUninstalling = "uninstalling"

	// Release conditions.
	ReleaseConditionReady          = "Ready"
	ReleaseConditionHooksSucceeded = "HooksSucceeded"
	ReleaseConditionDrifted        = "Drifted"

	// Deletion propagation of releases.
	ReleasePropagationBackground = "background"
	ReleasePropagationOrphan     = "orphan"

	// Job meta keys of hooks.
	ReleaseMetaHook        = "pack.hook"
	ReleaseMetaHookWeight  = "pack.hook_weight"
	ReleaseMetaHookDelete  = "pack.hook_delete"
	ReleaseMetaHookTimeout = "pack.hook_timeout"
	ReleaseMetaRelease     = "pack.release"
)

// Release is an installation of a pack.
type Release struct {
	ID        string
	Namespace string
	Name      string

	Spec   *ReleaseSpec
	Status *ReleaseStatus

	CreateTime  int64
	ModifyTime  int64
	CreateIndex uint64
	ModifyIndex uint64
}

// ReleaseSpec is the desired state of a release.
type ReleaseSpec struct {
	Pack        *ReleasePack `json:"pack,omitempty"`
	Description string       `json:"description,omitempty"`
	Revision    uint64       `json:"revision"`
	MaxHistory  int          `json:"maxHistory"`
	Atomic      bool         `json:"atomic"`
}

// ReleaseStatus is the observed state of a release.
type ReleaseStatus struct {
	Phase         string                `json:"phase"`
	Revision      uint64                `json:"revision"`
	Resources     []*ReleaseResourceRef `json:"resources,omitempty"`
	Conditions    []*ReleaseCondition   `json:"conditions,omitempty"`
	LastOperation *ReleaseOperation     `json:"lastOperation,omitempty"`
	LastTest      *ReleaseTestResult    `json:"lastTest,omitempty"`
}

// Condition returns the condition of the given type, or nil.
func (s *ReleaseStatus) Condition(t string) *ReleaseCondition {
	if s == nil {
		return nil
	}
	for _, c := range s.Conditions {
		if c != nil && c.Type == t {
			return c
		}
	}
	return nil
}

// ReleasePack describes the pack of a release.
type ReleasePack struct {
	Name          string                  `json:"name"`
	Version       string                  `json:"version,omitempty"`
	Registry      string                  `json:"registry,omitempty"`
	Path          string                  `json:"path,omitempty"`
	Digest        string                  `json:"digest,omitempty"`
	Signer        string                  `json:"signer,omitempty"`
	Signatures    []*ReleasePackSignature `json:"signatures,omitempty"`
	TrustedSigner string                  `json:"trustedSigner,omitempty"`
	Requires      *ReleasePackRequires    `json:"requires,omitempty"`
}

// ReleasePackSignature is a cosign signature of the pack digest.
type ReleasePackSignature struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// ReleasePackRequires are the requirements of a pack on the servers.
type ReleasePackRequires struct {
	Norca    string   `json:"norca,omitempty"`
	Features []string `json:"features,omitempty"`
}

// ReleaseResourceRef identifies a resource of a release.
type ReleaseResourceRef struct {
	Type        string `json:"type"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	Hook        string `json:"hook,omitempty"`
	ModifyIndex uint64 `json:"modifyIndex,omitempty"`
	Version     uint64 `json:"version,omitempty"`
	Hash        string `json:"hash,omitempty"`
}

// ReleaseCondition is a condition of a release.
type ReleaseCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// ReleaseOperation describes an operation on a release.
type ReleaseOperation struct {
	Action     string   `json:"action"`
	Revision   uint64   `json:"revision,omitempty"`
	User       string   `json:"user,omitempty"`
	StartTime  string   `json:"startTime,omitempty"`
	EndTime    string   `json:"endTime,omitempty"`
	Error      string   `json:"error,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	RolledBack bool     `json:"rolledBack,omitempty"`
}

// ReleaseTestResult is the result of the test hooks of a release.
type ReleaseTestResult struct {
	Revision  uint64               `json:"revision"`
	StartTime string               `json:"startTime,omitempty"`
	EndTime   string               `json:"endTime,omitempty"`
	Succeeded bool                 `json:"succeeded"`
	Hooks     []*ReleaseHookResult `json:"hooks,omitempty"`
}

// ReleaseHookResult is the result of a hook job.
type ReleaseHookResult struct {
	Phase     string `json:"phase"`
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}

// ReleaseRevision is an immutable revision of a release.
type ReleaseRevision struct {
	Namespace string
	Release   string
	Revision  uint64
	Spec      *ReleaseRevisionSpec
	Status    *ReleaseRevisionStatus

	// Resources are only returned by Revision.
	Resources []*ReleaseResource

	CreateIndex uint64
	ModifyIndex uint64
}

// ReleaseRevisionSpec is the content of a revision.
type ReleaseRevisionSpec struct {
	Release       string                `json:"release"`
	Revision      uint64                `json:"revision"`
	Action        string                `json:"action"`
	RollbackOf    uint64                `json:"rollbackOf,omitempty"`
	Pack          *ReleasePack          `json:"pack,omitempty"`
	Description   string                `json:"description,omitempty"`
	Values        map[string]any        `json:"values,omitempty"`
	ValuesHash    string                `json:"valuesHash,omitempty"`
	Resources     []*ReleaseResourceRef `json:"resources,omitempty"`
	ContentDigest string                `json:"contentDigest"`
	User          string                `json:"user,omitempty"`
	CreateTime    string                `json:"createTime,omitempty"`
}

// ReleaseRevisionStatus is the state of a revision.
type ReleaseRevisionStatus struct {
	Phase   string               `json:"phase"`
	Message string               `json:"message,omitempty"`
	Hooks   []*ReleaseHookResult `json:"hooks,omitempty"`
}

// ReleaseResource is a rendered resource of a pack. Exactly one payload
// matching Type is set.
type ReleaseResource struct {
	Type   string
	Source string `json:",omitempty"`

	Namespace       *Namespace       `json:",omitempty"`
	Quota           *QuotaSpec       `json:",omitempty"`
	ACLPolicy       *ACLPolicy       `json:",omitempty"`
	Variable        *Variable        `json:",omitempty"`
	ObjectKind      *ObjectKind      `json:",omitempty"`
	AdmissionPolicy *AdmissionPolicy `json:",omitempty"`
	Object          *CustomObject    `json:",omitempty"`
	Job             *Job             `json:",omitempty"`
	Submission      *JobSubmission   `json:",omitempty"`

	// SecretKeys are Variables items whose values are not stored in the
	// release; supply them with ReleaseApplyRequest.Secrets.
	SecretKeys []string `json:",omitempty"`
}

// ReleaseVariableSecret carries secret Variables items.
type ReleaseVariableSecret struct {
	Namespace string
	Path      string
	Items     map[string]string
}

// ReleaseApplyRequest installs, upgrades or plans a release.
type ReleaseApplyRequest struct {
	Pack        *ReleasePack
	Description string
	Values      map[string]any
	ValuesHash  string
	Resources   []*ReleaseResource
	Secrets     []*ReleaseVariableSecret

	// Atomic restores the previous state on failure; nil lets the
	// namespace decide (production namespaces are atomic).
	Atomic *bool

	// Wait waits up to Timeout for the deployments of the jobs.
	Wait    bool
	Timeout time.Duration

	TakeOwnership bool
	MaxHistory    int

	// RecordOnly records resources the client applied itself.
	RecordOnly bool
}

// ReleaseApplyResponse is the result of an apply, plan or rollback.
type ReleaseApplyResponse struct {
	Release  *Release
	Revision uint64
	Changes  []*ReleaseResourceChange
	Warnings []string
	WriteMeta
}

// ReleaseResourceChange is a planned or performed change.
type ReleaseResourceChange struct {
	Type      string
	Namespace string
	Name      string
	Action    string
	Hook      string
	Owner     string
	JobDiff   *JobDiff
	Error     string
}

// ReleaseRollbackRequest rolls back to a revision (0: the previous one).
type ReleaseRollbackRequest struct {
	Revision uint64
	Atomic   *bool
	Wait     bool
	Timeout  time.Duration
	DryRun   bool
}

// ReleaseDeleteOptions are the options of an uninstall.
type ReleaseDeleteOptions struct {
	// Propagation is "background" (delete the resources) or "orphan".
	Propagation string
	NoHooks     bool
	Timeout     time.Duration
}

// ReleaseDeleteResponse is the result of an uninstall.
type ReleaseDeleteResponse struct {
	Release *Release
	Changes []*ReleaseResourceChange
	WriteMeta
}

// ReleaseDriftResponse is the result of a drift check.
type ReleaseDriftResponse struct {
	Drifted   bool
	Revision  uint64
	Resources []*ReleaseDriftEntry
}

// ReleaseDriftEntry is a resource that drifted from the revision.
type ReleaseDriftEntry struct {
	Type      string `json:"type"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// ErrReleasesNotSupported is returned when the servers have no release API.
var ErrReleasesNotSupported = errors.New("the servers do not support pack releases")

// Releases is used to manage pack releases.
type Releases struct {
	client *Client
}

// Releases returns a handle on the release endpoints.
func (c *Client) Releases() *Releases {
	return &Releases{client: c}
}

func releasePath(name string, sub ...string) string {
	p := "/v1/release/" + url.PathEscape(name)
	for _, s := range sub {
		p += "/" + s
	}
	return p
}

// Supported reports whether the servers provide the release API.
func (r *Releases) Supported(q *QueryOptions) (bool, error) {
	var resp []*Release
	_, err := r.client.query("/v1/releases", &resp, q)
	if err == nil {
		return true, nil
	}
	var uerr UnexpectedResponseError
	if errors.As(err, &uerr) && (uerr.StatusCode() == 404 || uerr.StatusCode() == 405) {
		return false, nil
	}
	return false, err
}

// List lists the releases of the namespace ("*" for all).
func (r *Releases) List(q *QueryOptions) ([]*Release, *QueryMeta, error) {
	var resp []*Release
	qm, err := r.client.query("/v1/releases", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// Info returns a release.
func (r *Releases) Info(name string, q *QueryOptions) (*Release, *QueryMeta, error) {
	var resp Release
	qm, err := r.client.query(releasePath(name), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Apply installs or upgrades a release.
func (r *Releases) Apply(name string, req *ReleaseApplyRequest, w *WriteOptions) (*ReleaseApplyResponse, *WriteMeta, error) {
	var resp ReleaseApplyResponse
	wm, err := r.client.put(releasePath(name), req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Plan plans an install or upgrade without changing anything.
func (r *Releases) Plan(name string, req *ReleaseApplyRequest, w *WriteOptions) (*ReleaseApplyResponse, *WriteMeta, error) {
	var resp ReleaseApplyResponse
	wm, err := r.client.post(releasePath(name, "plan"), req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// History lists the revisions of a release, newest first.
func (r *Releases) History(name string, q *QueryOptions) ([]*ReleaseRevision, *QueryMeta, error) {
	var resp []*ReleaseRevision
	qm, err := r.client.query(releasePath(name, "history"), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// Revision returns a revision with its resources.
func (r *Releases) Revision(name string, revision uint64, q *QueryOptions) (*ReleaseRevision, *QueryMeta, error) {
	var resp ReleaseRevision
	qm, err := r.client.query(releasePath(name, "revision", strconv.FormatUint(revision, 10)), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Rollback applies an earlier revision as a new revision.
func (r *Releases) Rollback(name string, req *ReleaseRollbackRequest, w *WriteOptions) (*ReleaseApplyResponse, *WriteMeta, error) {
	if req == nil {
		req = &ReleaseRollbackRequest{}
	}
	var resp ReleaseApplyResponse
	wm, err := r.client.post(releasePath(name, "rollback"), req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Test runs the test hooks of a release.
func (r *Releases) Test(name string, timeout time.Duration, w *WriteOptions) (*ReleaseTestResult, *WriteMeta, error) {
	path := releasePath(name, "test")
	if timeout > 0 {
		path += "?timeout=" + url.QueryEscape(timeout.String())
	}
	var resp ReleaseTestResult
	wm, err := r.client.post(path, nil, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Drift checks a release for drift now.
func (r *Releases) Drift(name string, w *WriteOptions) (*ReleaseDriftResponse, *WriteMeta, error) {
	var resp ReleaseDriftResponse
	wm, err := r.client.post(releasePath(name, "drift"), nil, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Delete uninstalls a release.
func (r *Releases) Delete(name string, opts *ReleaseDeleteOptions, w *WriteOptions) (*ReleaseDeleteResponse, *WriteMeta, error) {
	path := releasePath(name)
	if opts != nil {
		v := url.Values{}
		if opts.Propagation != "" {
			v.Set("propagation", opts.Propagation)
		}
		if opts.NoHooks {
			v.Set("no_hooks", "true")
		}
		if opts.Timeout > 0 {
			v.Set("timeout", opts.Timeout.String())
		}
		if len(v) > 0 {
			path += "?" + v.Encode()
		}
	}
	var resp ReleaseDeleteResponse
	wm, err := r.client.delete(path, nil, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// String returns "type namespace/name" of a resource reference.
func (r *ReleaseResourceRef) String() string {
	if r.Namespace != "" {
		return fmt.Sprintf("%s %s/%s", r.Type, r.Namespace, r.Name)
	}
	return fmt.Sprintf("%s %s", r.Type, r.Name)
}
