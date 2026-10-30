// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/go-multierror"
)

// Pack releases (package management plan, phases 2 and 3): a Release
// records an installation of a pack with its immutable revisions, and the
// Release RPCs apply the rendered resources of a pack on the server. Both are
// stored as custom objects of built-in kinds, so they share the storage, the
// ACL model, the event topic and the backups of custom objects.

const (
	// ReleaseObjectGroup is the object group of the built-in release kinds.
	// Its kinds can't be written through the generic object API.
	ReleaseObjectGroup = "packs.norca.io"

	// ReleaseKind is the kind of release objects, ReleaseRevisionKind the
	// kind of their revisions, and PackRegistryKind the kind of cluster
	// pack registries.
	ReleaseKind         = "Release"
	ReleaseRevisionKind = "ReleaseRevision"
	PackRegistryKind    = "Registry"

	// PackUploadKind is the kind of the chunks of packs uploaded to a
	// cluster registry of the type "uploads".
	PackUploadKind = "PackUpload"

	// ReleaseLabel is the label of revisions naming their release.
	ReleaseLabel = "packs.norca.io/release"

	// ReleaseManagedBy is the managed-by value of release objects.
	ReleaseManagedBy = "norca-release"

	// DefaultReleaseMaxHistory is the default number of revisions kept per
	// release (decided 2026-10-24); MaxReleaseMaxHistory the upper bound.
	DefaultReleaseMaxHistory = 10
	MaxReleaseMaxHistory     = 100

	// DefaultReleaseTimeout bounds an operation that waits for deployments
	// and hooks; MaxReleaseTimeout is the largest timeout accepted.
	DefaultReleaseTimeout = 5 * time.Minute
	MaxReleaseTimeout     = 2 * time.Hour

	// DefaultReleaseHookTimeout bounds a single hook job.
	DefaultReleaseHookTimeout = 5 * time.Minute

	// MaxReleaseResources is the maximum number of resources of a release.
	MaxReleaseResources = 256

	// MaxReleaseRevisionContentSize bounds the compressed, encoded content of
	// a revision so that the revision object stays below the object size
	// limit.
	MaxReleaseRevisionContentSize = MaxCustomObjectSize - 32*1024

	// maxReleaseNameLength leaves room for the revision suffix in the names
	// of revision objects.
	maxReleaseNameLength = 100

	// MaxReleaseVariablesSchemaSize bounds the total size of the variables
	// schemas of a release, which every revision stores next to its content.
	MaxReleaseVariablesSchemaSize = 64 * 1024

	// MaxReleaseRegions is the maximum number of regions of a multi-region
	// install.
	MaxReleaseRegions = 32
)

// Phases of releases and revisions.
const (
	ReleasePhasePending      = "pending"
	ReleasePhaseApplying     = "applying"
	ReleasePhaseDeployed     = "deployed"
	ReleasePhaseFailed       = "failed"
	ReleasePhaseRollingBack  = "rolling-back"
	ReleasePhaseSuperseded   = "superseded"
	ReleasePhaseUninstalling = "uninstalling"
)

// Actions of release operations.
const (
	ReleaseActionInstall   = "install"
	ReleaseActionUpgrade   = "upgrade"
	ReleaseActionRollback  = "rollback"
	ReleaseActionUninstall = "uninstall"
	ReleaseActionTest      = "test"
)

// Condition types and statuses of releases.
const (
	ReleaseConditionReady          = "Ready"
	ReleaseConditionHooksSucceeded = "HooksSucceeded"
	ReleaseConditionDrifted        = "Drifted"

	ReleaseConditionTrue    = "True"
	ReleaseConditionFalse   = "False"
	ReleaseConditionUnknown = "Unknown"
)

// Resource types of a release, in the order they are applied (deletion uses
// the reverse order).
const (
	ReleaseResourceNamespace       = "namespace"
	ReleaseResourceQuota           = "quota"
	ReleaseResourceACLPolicy       = "acl-policy"
	ReleaseResourceVariable        = "variable"
	ReleaseResourceObjectKind      = "object-kind"
	ReleaseResourceAdmissionPolicy = "admission-policy"
	ReleaseResourceObject          = "object"
	ReleaseResourceJob             = "job"
)

// IsGlobalReleaseResource reports whether resources of the type are global:
// the servers write them in the authoritative region and replicate them to
// the other regions (namespaces, quotas and ACL policies).
func IsGlobalReleaseResource(t string) bool {
	switch t {
	case ReleaseResourceNamespace, ReleaseResourceQuota, ReleaseResourceACLPolicy:
		return true
	}
	return false
}

// releaseResourceOrder is the application order of the resource types.
var releaseResourceOrder = map[string]int{
	ReleaseResourceNamespace:       1,
	ReleaseResourceQuota:           2,
	ReleaseResourceACLPolicy:       3,
	ReleaseResourceVariable:        4,
	ReleaseResourceObjectKind:      5,
	ReleaseResourceAdmissionPolicy: 6,
	ReleaseResourceObject:          7,
	ReleaseResourceJob:             8,
}

// Job meta keys of releases. The pack.release keys mark the jobs a release
// owns; the hook keys turn a batch job of a pack into a hook.
const (
	ReleaseMetaRelease          = "pack.release"
	ReleaseMetaReleaseNamespace = "pack.release_namespace"

	ReleaseMetaHook        = "pack.hook"
	ReleaseMetaHookWeight  = "pack.hook_weight"
	ReleaseMetaHookDelete  = "pack.hook_delete"
	ReleaseMetaHookTimeout = "pack.hook_timeout"
)

// Hook phases.
const (
	ReleaseHookPreInstall  = "pre-install"
	ReleaseHookPostInstall = "post-install"
	ReleaseHookPreUpgrade  = "pre-upgrade"
	ReleaseHookPostUpgrade = "post-upgrade"
	ReleaseHookPreDelete   = "pre-delete"
	ReleaseHookPostDelete  = "post-delete"
	ReleaseHookTest        = "test"
)

var releaseHookPhases = []string{
	ReleaseHookPreInstall, ReleaseHookPostInstall, ReleaseHookPreUpgrade, ReleaseHookPostUpgrade,
	ReleaseHookPreDelete, ReleaseHookPostDelete, ReleaseHookTest,
}

// Deletion policies of hook jobs.
const (
	ReleaseHookDeleteOnSuccess = "on-success"
	ReleaseHookDeleteAlways    = "always"
	ReleaseHookDeleteNever     = "never"
)

// Deletion propagation of releases: background deletes the resources of the
// release, orphan keeps them.
const (
	ReleasePropagationBackground = "background"
	ReleasePropagationOrphan     = "orphan"
)

const (
	// NamespaceMetaEnvironment is the namespace meta key that marks
	// production namespaces: the values "production" and "prod" (in any
	// case) make atomic mode the default for releases in the namespace.
	NamespaceMetaEnvironment = "environment"

	// NamespaceMetaReleaseAtomic overrides the atomic default of a
	// namespace ("true" or "false").
	NamespaceMetaReleaseAtomic = "pack.atomic"
)

// TypeReleaseDrifted is the event type published on the CustomObject topic
// when a release starts to drift from its revision.
const TypeReleaseDrifted = "ReleaseDrifted"

// ReleaseKindRef is the "group/Kind" reference of releases.
var ReleaseKindRef = ObjectKindRef(ReleaseObjectGroup, ReleaseKind)

// ReleaseRevisionKindRef is the "group/Kind" reference of revisions.
var ReleaseRevisionKindRef = ObjectKindRef(ReleaseObjectGroup, ReleaseRevisionKind)

// PackRegistryKindRef is the "group/Kind" reference of pack registries.
var PackRegistryKindRef = ObjectKindRef(ReleaseObjectGroup, PackRegistryKind)

// PackUploadKindRef is the "group/Kind" reference of pack upload chunks.
var PackUploadKindRef = ObjectKindRef(ReleaseObjectGroup, PackUploadKind)

// IsReleaseHookPhase reports whether phase is a hook phase.
func IsReleaseHookPhase(phase string) bool {
	return slices.Contains(releaseHookPhases, phase)
}

// ReleaseAtomicDefault returns whether operations on releases in the
// namespace are atomic unless the request says otherwise: namespaces with
// the meta pack.atomic use that, production namespaces (meta environment =
// "production" or "prod") default to atomic, others do not.
func ReleaseAtomicDefault(ns *Namespace) bool {
	if ns == nil {
		return false
	}
	if v, ok := ns.Meta[NamespaceMetaReleaseAtomic]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	switch strings.ToLower(ns.Meta[NamespaceMetaEnvironment]) {
	case "production", "prod":
		return true
	}
	return false
}

// ValidateReleaseName returns an error if name is not a valid release name.
func ValidateReleaseName(name string) error {
	if len(name) > maxReleaseNameLength || !validCustomObjectName.MatchString(name) {
		return fmt.Errorf("invalid release name %q, must match regex %s and have at most %d characters",
			name, validCustomObjectName, maxReleaseNameLength)
	}
	return nil
}

// ReleaseRevisionObjectName returns the name of the object of a revision.
func ReleaseRevisionObjectName(release string, revision uint64) string {
	return release + "." + strconv.FormatUint(revision, 10)
}

// Release is an installation of a pack. It is the typed view of a custom
// object of the kind packs.norca.io/Release: Spec and Status are the spec
// and the status of the object.
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
	// Pack describes the pack of the newest revision.
	Pack *ReleasePack `json:"pack,omitempty"`

	// Description is the description of the newest revision.
	Description string `json:"description,omitempty"`

	// Revision is the number of the newest revision.
	Revision uint64 `json:"revision"`

	// MaxHistory is the number of revisions kept.
	MaxHistory int `json:"maxHistory"`

	// Atomic reports whether the last operation was atomic.
	Atomic bool `json:"atomic"`

	// Multiregion describes the multi-region install of the newest revision,
	// nil for a release installed in its region only.
	Multiregion *ReleaseMultiregion `json:"multiregion,omitempty"`
}

// ReleaseMultiregion describes a multi-region install: the same pack
// applied as a release of the same name in every listed region, on the
// leader of each region. The releases are independent objects of their
// regions; this records how they share the work.
type ReleaseMultiregion struct {
	// Regions are the regions of the install, in the order they are
	// applied: GlobalRegion first, then the others as requested.
	Regions []string `json:"regions"`

	// GlobalRegion is the region whose release applies and owns the global
	// resources of the pack (namespaces, quotas, ACL policies): the
	// authoritative region if it is one of the regions, else the first
	// region requested. The servers replicate them to the other regions.
	GlobalRegion string `json:"globalRegion"`
}

// Validate checks the description of a multi-region install as received by
// the given region.
func (m *ReleaseMultiregion) Validate(region string) error {
	if len(m.Regions) == 0 || len(m.Regions) > MaxReleaseRegions {
		return fmt.Errorf("a multi-region install needs between 1 and %d regions", MaxReleaseRegions)
	}
	seen := map[string]bool{}
	for _, r := range m.Regions {
		if r == "" || seen[r] {
			return fmt.Errorf("invalid or duplicate region %q", r)
		}
		seen[r] = true
	}
	if !seen[region] {
		return fmt.Errorf("region %q is not one of the regions of the install (%s)", region, strings.Join(m.Regions, ", "))
	}
	if m.GlobalRegion != m.Regions[0] {
		return fmt.Errorf("the global region %q must be the first region of the install", m.GlobalRegion)
	}
	return nil
}

// Copy returns a copy.
func (m *ReleaseMultiregion) Copy() *ReleaseMultiregion {
	if m == nil {
		return nil
	}
	return &ReleaseMultiregion{Regions: slices.Clone(m.Regions), GlobalRegion: m.GlobalRegion}
}

// JobSubmitter returns the region whose release registers a job with a
// multiregion block that lists jobRegions: the first region of the install
// the job lists, or else the global region. That region submits the job to
// all its regions (Norca's multiregion deployment); the releases of the
// other regions the job lists adopt their copy.
func (m *ReleaseMultiregion) JobSubmitter(jobRegions []string) string {
	for _, r := range m.Regions {
		if slices.Contains(jobRegions, r) {
			return r
		}
	}
	return m.GlobalRegion
}

// ReleaseVariablesSchema is the JSON Schema of the variables of a pack or of
// one of its dependencies (variables_schema in metadata.hcl). The schema
// describes the object of the variable values by variable name.
type ReleaseVariablesSchema struct {
	// Pack is the path of the pack in the dependency tree, e.g. "redis" or
	// "app.redis".
	Pack string `json:"pack"`

	// Schema is the JSON Schema document.
	Schema string `json:"schema"`
}

// ReleaseStatus is the observed state of a release.
type ReleaseStatus struct {
	// Phase is the phase of the release.
	Phase string `json:"phase"`

	// Revision is the revision whose resources are deployed (after a failed
	// atomic upgrade, the previous revision).
	Revision uint64 `json:"revision"`

	// Resources are the resources the release owns.
	Resources []*ReleaseResourceRef `json:"resources,omitempty"`

	// Conditions are Ready, HooksSucceeded and Drifted.
	Conditions []*ReleaseCondition `json:"conditions,omitempty"`

	// LastOperation describes the most recent operation.
	LastOperation *ReleaseOperation `json:"lastOperation,omitempty"`

	// LastTest is the result of the most recent test run.
	LastTest *ReleaseTestResult `json:"lastTest,omitempty"`
}

// ReleasePack describes the pack of a release revision.
type ReleasePack struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Registry string `json:"registry,omitempty"`
	Path     string `json:"path,omitempty"`

	// Source is where the registry of the pack fetches it from (a git URL
	// or an oci:// location), and Ref the ref (tag, branch) it was fetched
	// at. With them the web UI finds the cluster registry of a release
	// installed with the CLI, whose Registry is a local name.
	Source string `json:"source,omitempty"`
	Ref    string `json:"ref,omitempty"`

	// Digest is the OCI manifest digest of a pack from an OCI registry or
	// a bundle.
	Digest string `json:"digest,omitempty"`

	// Signer is the key ID of the trusted key the client verified the
	// signature with. It is reported by the client.
	Signer string `json:"signer,omitempty"`

	// Signatures are the cosign signatures of the digest, so that the server
	// can verify them against the trusted keys of admission policies.
	Signatures []*ReleasePackSignature `json:"signatures,omitempty"`

	// TrustedSigner is the key ID of the key the server verified a signature
	// with, while admitting the release.
	TrustedSigner string `json:"trustedSigner,omitempty"`

	// Requires are the requirements of the pack on the server.
	Requires *ReleasePackRequires `json:"requires,omitempty"`
}

// ReleasePackSignature is a cosign "simple signing" signature: the payload
// and the base64 signature of its SHA-256.
type ReleasePackSignature struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// ReleasePackRequires are the requirements a pack states in metadata.hcl.
type ReleasePackRequires struct {
	// Norca is a version constraint on the Norca version of the servers.
	Norca string `json:"norca,omitempty"`

	// Features are server features the pack uses.
	Features []string `json:"features,omitempty"`
}

// ReleaseResourceRef identifies a resource of a release.
type ReleaseResourceRef struct {
	Type string `json:"type"`

	// Namespace is the namespace of namespaced resources.
	Namespace string `json:"namespace,omitempty"`

	// Name is the job ID, the Variables path, the "group/Kind" of an object
	// kind, the "group/Kind/name" of an object, or the name of the
	// resource.
	Name string `json:"name"`

	// Hook is the hook phases of a hook job.
	Hook string `json:"hook,omitempty"`

	// ModifyIndex is the modify index of the resource after it was applied,
	// and Version the job version or object generation.
	ModifyIndex uint64 `json:"modifyIndex,omitempty"`
	Version     uint64 `json:"version,omitempty"`

	// Hash is the hash of the resource after it was applied, used for drift
	// detection.
	Hash string `json:"hash,omitempty"`
}

// Key returns the identity of the resource: "type/namespace/name".
func (r *ReleaseResourceRef) Key() string {
	return r.Type + "/" + r.Namespace + "/" + r.Name
}

// String returns a human-friendly name of the resource.
func (r *ReleaseResourceRef) String() string {
	if r.Namespace != "" {
		return fmt.Sprintf("%s %q (namespace %q)", r.Type, r.Name, r.Namespace)
	}
	return fmt.Sprintf("%s %q", r.Type, r.Name)
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
	Action    string   `json:"action"`
	Revision  uint64   `json:"revision,omitempty"`
	User      string   `json:"user,omitempty"`
	StartTime string   `json:"startTime,omitempty"`
	EndTime   string   `json:"endTime,omitempty"`
	Error     string   `json:"error,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`

	// RolledBack reports that an atomic operation failed and the resources
	// of the previous revision were restored.
	RolledBack bool `json:"rolledBack,omitempty"`
}

// ReleaseTestResult is the result of running the test hooks of a release.
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

	// Status is "succeeded", "failed" or "timeout".
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Statuses of hook results.
const (
	ReleaseHookSucceeded = "succeeded"
	ReleaseHookFailed    = "failed"
	ReleaseHookTimeout   = "timeout"
)

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

// SetCondition sets a condition. The transition time changes only when the
// status changes. It reports whether the status changed.
func (s *ReleaseStatus) SetCondition(t, status, reason, message string, now time.Time) bool {
	c := s.Condition(t)
	if c == nil {
		s.Conditions = append(s.Conditions, &ReleaseCondition{
			Type: t, Status: status, Reason: reason, Message: message,
			LastTransitionTime: now.UTC().Format(time.RFC3339),
		})
		return true
	}
	changed := c.Status != status
	if changed {
		c.LastTransitionTime = now.UTC().Format(time.RFC3339)
	}
	c.Status, c.Reason, c.Message = status, reason, message
	return changed
}

// ReleaseRevision is an immutable revision of a release: the rendered
// resources of an install, upgrade or rollback. It is the typed view of a
// custom object of the kind packs.norca.io/ReleaseRevision.
type ReleaseRevision struct {
	Namespace string
	Release   string
	Revision  uint64

	Spec   *ReleaseRevisionSpec
	Status *ReleaseRevisionStatus

	// Resources is the decoded content. It is only set in responses that
	// ask for a single revision.
	Resources []*ReleaseResource

	CreateIndex uint64
	ModifyIndex uint64
}

// ReleaseRevisionSpec is the immutable content of a revision.
type ReleaseRevisionSpec struct {
	Release  string `json:"release"`
	Revision uint64 `json:"revision"`

	// Action is install, upgrade or rollback; RollbackOf the revision a
	// rollback restored.
	Action     string `json:"action"`
	RollbackOf uint64 `json:"rollbackOf,omitempty"`

	Pack        *ReleasePack `json:"pack,omitempty"`
	Description string       `json:"description,omitempty"`

	// Values are the variable values of the pack, without secret values;
	// ValuesHash is the hash of all values computed by the client.
	Values     map[string]any `json:"values,omitempty"`
	ValuesHash string         `json:"valuesHash,omitempty"`

	// VariablesSchemas are the variables schemas of the pack and its
	// dependencies the values were validated against.
	VariablesSchemas []*ReleaseVariablesSchema `json:"variablesSchemas,omitempty"`

	// Multiregion describes the multi-region install the revision belongs
	// to; a rollback applies the revision with the same division of work.
	Multiregion *ReleaseMultiregion `json:"multiregion,omitempty"`

	// Resources lists the resources of the revision.
	Resources []*ReleaseResourceRef `json:"resources,omitempty"`

	// Content is the base64 encoded, gzip compressed msgpack encoding of the
	// resources, and ContentDigest the SHA-256 of the uncompressed encoding.
	Content       string `json:"content"`
	ContentDigest string `json:"contentDigest"`

	User       string `json:"user,omitempty"`
	CreateTime string `json:"createTime,omitempty"`
}

// ReleaseRevisionStatus is the state of a revision.
type ReleaseRevisionStatus struct {
	Phase   string               `json:"phase"`
	Message string               `json:"message,omitempty"`
	Hooks   []*ReleaseHookResult `json:"hooks,omitempty"`
}

// ReleaseResource is a rendered resource of a pack. Exactly one of the
// resource fields is set, matching Type.
type ReleaseResource struct {
	Type string

	// Source is the template the resource was rendered from.
	Source string

	Namespace       *Namespace
	Quota           *QuotaSpec
	ACLPolicy       *ACLPolicy
	Variable        *VariableDecrypted
	ObjectKind      *ObjectKind
	AdmissionPolicy *AdmissionPolicy
	Object          *CustomObject
	Job             *Job

	// SecretKeys are the items of a Variable whose values are secret: they
	// are never stored in the release; their values come from the request
	// or are kept from the stored Variable.
	SecretKeys []string

	// Submission is the source of a job.
	Submission *JobSubmission
}

// ReleaseVariableSecret carries the secret items of a Variable of a
// release. It is applied but never stored in the release.
type ReleaseVariableSecret struct {
	Namespace string
	Path      string
	Items     map[string]string
}

// Order returns the application order of the resource.
func (r *ReleaseResource) Order() int {
	return releaseResourceOrder[r.Type]
}

// Ref returns the reference of the resource. Namespaces of namespaced
// resources must have been set.
func (r *ReleaseResource) Ref() *ReleaseResourceRef {
	ref := &ReleaseResourceRef{Type: r.Type}
	switch r.Type {
	case ReleaseResourceNamespace:
		if r.Namespace != nil {
			ref.Name = r.Namespace.Name
		}
	case ReleaseResourceQuota:
		if r.Quota != nil {
			ref.Name = r.Quota.Name
		}
	case ReleaseResourceACLPolicy:
		if r.ACLPolicy != nil {
			ref.Name = r.ACLPolicy.Name
		}
	case ReleaseResourceVariable:
		if r.Variable != nil {
			ref.Namespace, ref.Name = r.Variable.Namespace, r.Variable.Path
		}
	case ReleaseResourceObjectKind:
		if r.ObjectKind != nil {
			ref.Name = r.ObjectKind.KindRef()
		}
	case ReleaseResourceAdmissionPolicy:
		if r.AdmissionPolicy != nil {
			ref.Name = r.AdmissionPolicy.Name
		}
	case ReleaseResourceObject:
		if r.Object != nil {
			ref.Namespace, ref.Name = r.Object.Namespace, r.Object.KindRef()+"/"+r.Object.Name
		}
	case ReleaseResourceJob:
		if r.Job != nil {
			ref.Namespace, ref.Name = r.Job.Namespace, r.Job.ID
			ref.Hook = r.HookPhases()
		}
	}
	return ref
}

// HookPhases returns the hook phases of a hook job ("" for other
// resources), as given in the pack.hook meta (comma separated).
func (r *ReleaseResource) HookPhases() string {
	if r.Type != ReleaseResourceJob || r.Job == nil {
		return ""
	}
	return strings.Join(splitReleaseHooks(r.Job.Meta[ReleaseMetaHook]), ",")
}

// IsHook reports whether the resource is a hook job.
func (r *ReleaseResource) IsHook() bool {
	return r.HookPhases() != ""
}

// HasHookPhase reports whether the resource is a hook job of the phase.
func (r *ReleaseResource) HasHookPhase(phase string) bool {
	if r.Type != ReleaseResourceJob || r.Job == nil {
		return false
	}
	return slices.Contains(splitReleaseHooks(r.Job.Meta[ReleaseMetaHook]), phase)
}

// HookWeight returns the weight of a hook job (pack.hook_weight).
func (r *ReleaseResource) HookWeight() int {
	if r.Job == nil {
		return 0
	}
	w, _ := strconv.Atoi(strings.TrimSpace(r.Job.Meta[ReleaseMetaHookWeight]))
	return w
}

// HookDeletePolicy returns the deletion policy of a hook job.
func (r *ReleaseResource) HookDeletePolicy() string {
	if r.Job == nil || r.Job.Meta[ReleaseMetaHookDelete] == "" {
		return ReleaseHookDeleteOnSuccess
	}
	return r.Job.Meta[ReleaseMetaHookDelete]
}

// HookTimeout returns the timeout of a hook job (pack.hook_timeout).
func (r *ReleaseResource) HookTimeout() time.Duration {
	if r.Job != nil {
		if d, err := time.ParseDuration(r.Job.Meta[ReleaseMetaHookTimeout]); err == nil && d > 0 {
			return d
		}
	}
	return DefaultReleaseHookTimeout
}

func splitReleaseHooks(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks that the resource carries the payload of its type and
// that the payload identifies the resource. The payload itself is validated
// by the endpoint it is applied with.
func (r *ReleaseResource) Validate() error {
	payloads := 0
	for _, set := range []bool{
		r.Namespace != nil, r.Quota != nil, r.ACLPolicy != nil, r.Variable != nil,
		r.ObjectKind != nil, r.AdmissionPolicy != nil, r.Object != nil, r.Job != nil,
	} {
		if set {
			payloads++
		}
	}
	if payloads != 1 {
		return fmt.Errorf("resource of type %q must have exactly one payload, has %d", r.Type, payloads)
	}

	var missing bool
	switch r.Type {
	case ReleaseResourceNamespace:
		missing = r.Namespace == nil || r.Namespace.Name == ""
	case ReleaseResourceQuota:
		missing = r.Quota == nil || r.Quota.Name == ""
	case ReleaseResourceACLPolicy:
		missing = r.ACLPolicy == nil || r.ACLPolicy.Name == ""
	case ReleaseResourceVariable:
		missing = r.Variable == nil || r.Variable.Path == ""
	case ReleaseResourceObjectKind:
		missing = r.ObjectKind == nil || r.ObjectKind.Group == "" || r.ObjectKind.Kind == ""
	case ReleaseResourceAdmissionPolicy:
		missing = r.AdmissionPolicy == nil || r.AdmissionPolicy.Name == ""
	case ReleaseResourceObject:
		missing = r.Object == nil || r.Object.Group == "" || r.Object.Kind == "" || r.Object.Name == ""
	case ReleaseResourceJob:
		missing = r.Job == nil || r.Job.ID == ""
	default:
		return fmt.Errorf("unknown resource type %q", r.Type)
	}
	if missing {
		return fmt.Errorf("resource of type %q has no payload or no name", r.Type)
	}
	if r.Type == ReleaseResourceObject && r.Object.Group == ReleaseObjectGroup {
		return fmt.Errorf("objects of the group %q can't be part of a pack", ReleaseObjectGroup)
	}
	if r.Type == ReleaseResourceObjectKind && r.ObjectKind.Group == ReleaseObjectGroup {
		return fmt.Errorf("object kinds of the group %q are built in", ReleaseObjectGroup)
	}
	if len(r.SecretKeys) > 0 && r.Type != ReleaseResourceVariable {
		return fmt.Errorf("only Variables can have secret items")
	}

	if r.Type == ReleaseResourceJob {
		hooks := splitReleaseHooks(r.Job.Meta[ReleaseMetaHook])
		for _, h := range hooks {
			if !IsReleaseHookPhase(h) {
				return fmt.Errorf("job %q: invalid hook %q, must be one of %s", r.Job.ID, h, strings.Join(releaseHookPhases, ", "))
			}
		}
		if len(hooks) > 0 {
			if r.Job.Type != JobTypeBatch && r.Job.Type != JobTypeSysBatch {
				return fmt.Errorf("hook job %q must be a batch or sysbatch job", r.Job.ID)
			}
			if r.Job.IsPeriodic() || r.Job.IsParameterized() {
				return fmt.Errorf("hook job %q can't be periodic or parameterized", r.Job.ID)
			}
			switch r.HookDeletePolicy() {
			case ReleaseHookDeleteOnSuccess, ReleaseHookDeleteAlways, ReleaseHookDeleteNever:
			default:
				return fmt.Errorf("job %q: invalid %s %q", r.Job.ID, ReleaseMetaHookDelete, r.Job.Meta[ReleaseMetaHookDelete])
			}
			if v := r.Job.Meta[ReleaseMetaHookTimeout]; v != "" {
				if d, err := time.ParseDuration(v); err != nil || d <= 0 {
					return fmt.Errorf("job %q: invalid %s %q", r.Job.ID, ReleaseMetaHookTimeout, v)
				}
			}
		}
	}
	return nil
}

// Copy returns a deep copy of the resource.
func (r *ReleaseResource) Copy() *ReleaseResource {
	if r == nil {
		return nil
	}
	nr := *r
	if r.Namespace != nil {
		nr.Namespace = r.Namespace.Copy()
	}
	if r.Quota != nil {
		nr.Quota = r.Quota.Copy()
	}
	if r.ACLPolicy != nil {
		p := *r.ACLPolicy
		if r.ACLPolicy.JobACL != nil {
			j := *r.ACLPolicy.JobACL
			p.JobACL = &j
		}
		p.Hash = slices.Clone(r.ACLPolicy.Hash)
		nr.ACLPolicy = &p
	}
	if r.Variable != nil {
		v := r.Variable.Copy()
		nr.Variable = &v
	}
	if r.ObjectKind != nil {
		nr.ObjectKind = r.ObjectKind.Copy()
	}
	if r.AdmissionPolicy != nil {
		nr.AdmissionPolicy = r.AdmissionPolicy.Copy()
	}
	if r.Object != nil {
		nr.Object = r.Object.Copy()
	}
	if r.Job != nil {
		nr.Job = r.Job.Copy()
	}
	if r.Submission != nil {
		nr.Submission = r.Submission.Copy()
	}
	nr.SecretKeys = slices.Clone(r.SecretKeys)
	return &nr
}

// SortReleaseResources sorts resources in application order: by type
// order, then namespace and name.
func SortReleaseResources(resources []*ReleaseResource) {
	sort.SliceStable(resources, func(i, j int) bool {
		a, b := resources[i], resources[j]
		if a.Order() != b.Order() {
			return a.Order() < b.Order()
		}
		ra, rb := a.Ref(), b.Ref()
		if ra.Namespace != rb.Namespace {
			return ra.Namespace < rb.Namespace
		}
		return ra.Name < rb.Name
	})
}

// ReleaseResourceTypeOrder returns the application order of a type (0 for
// unknown types).
func ReleaseResourceTypeOrder(t string) int {
	return releaseResourceOrder[t]
}

// EncodeReleaseResources encodes resources for the content of a revision:
// msgpack, gzip, base64. It also returns the hex SHA-256 of the msgpack
// encoding.
func EncodeReleaseResources(resources []*ReleaseResource) (content, digest string, err error) {
	var raw bytes.Buffer
	if err := codec.NewEncoder(&raw, MsgpackHandle).Encode(resources); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw.Bytes())

	var zipped bytes.Buffer
	zw, err := gzip.NewWriterLevel(&zipped, gzip.BestCompression)
	if err != nil {
		return "", "", err
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return "", "", err
	}
	if err := zw.Close(); err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(zipped.Bytes()), "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DecodeReleaseResources decodes the content of a revision.
func DecodeReleaseResources(content string) ([]*ReleaseResource, error) {
	zipped, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return nil, fmt.Errorf("invalid revision content: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, fmt.Errorf("invalid revision content: %w", err)
	}
	// The content is bounded by the object size; bound the expansion too.
	raw, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("invalid revision content: %w", err)
	}
	var resources []*ReleaseResource
	if err := codec.NewDecoder(bytes.NewReader(raw), MsgpackHandle).Decode(&resources); err != nil {
		return nil, fmt.Errorf("invalid revision content: %w", err)
	}
	return resources, nil
}

// releaseObjectMap converts a typed spec or status to the JSON-like map
// stored in a custom object.
func releaseObjectMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// releaseFromObjectMap converts the spec or status map of a custom object
// to a typed value.
func releaseFromObjectMap(m map[string]any, out any) error {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// ReleaseFromObject returns the typed view of a release object.
func ReleaseFromObject(obj *CustomObject) (*Release, error) {
	if obj == nil {
		return nil, nil
	}
	r := &Release{
		ID:          obj.ID,
		Namespace:   obj.Namespace,
		Name:        obj.Name,
		Spec:        &ReleaseSpec{},
		Status:      &ReleaseStatus{},
		CreateTime:  obj.CreateTime,
		ModifyTime:  obj.ModifyTime,
		CreateIndex: obj.CreateIndex,
		ModifyIndex: obj.ModifyIndex,
	}
	if err := releaseFromObjectMap(obj.Spec, r.Spec); err != nil {
		return nil, fmt.Errorf("release %q has an invalid spec: %w", obj.Name, err)
	}
	if err := releaseFromObjectMap(obj.Status, r.Status); err != nil {
		return nil, fmt.Errorf("release %q has an invalid status: %w", obj.Name, err)
	}
	return r, nil
}

// ObjectParts returns the spec and the status of the release object.
func (r *Release) ObjectParts() (spec, status map[string]any, err error) {
	if spec, err = releaseObjectMap(r.Spec); err != nil {
		return nil, nil, err
	}
	if r.Status != nil {
		if status, err = releaseObjectMap(r.Status); err != nil {
			return nil, nil, err
		}
	}
	return spec, status, nil
}

// Copy returns a deep copy of the release.
func (r *Release) Copy() *Release {
	if r == nil {
		return nil
	}
	nr := *r
	if r.Spec != nil {
		var s ReleaseSpec
		_ = releaseCopyJSON(r.Spec, &s)
		nr.Spec = &s
	}
	if r.Status != nil {
		var s ReleaseStatus
		_ = releaseCopyJSON(r.Status, &s)
		nr.Status = &s
	}
	return &nr
}

func releaseCopyJSON(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// ReleaseRevisionFromObject returns the typed view of a revision object.
// The content is not decoded.
func ReleaseRevisionFromObject(obj *CustomObject) (*ReleaseRevision, error) {
	if obj == nil {
		return nil, nil
	}
	rev := &ReleaseRevision{
		Namespace:   obj.Namespace,
		Release:     obj.Labels[ReleaseLabel],
		Spec:        &ReleaseRevisionSpec{},
		Status:      &ReleaseRevisionStatus{},
		CreateIndex: obj.CreateIndex,
		ModifyIndex: obj.ModifyIndex,
	}
	if err := releaseFromObjectMap(obj.Spec, rev.Spec); err != nil {
		return nil, fmt.Errorf("revision %q has an invalid spec: %w", obj.Name, err)
	}
	if err := releaseFromObjectMap(obj.Status, rev.Status); err != nil {
		return nil, fmt.Errorf("revision %q has an invalid status: %w", obj.Name, err)
	}
	rev.Revision = rev.Spec.Revision
	if rev.Release == "" {
		rev.Release = rev.Spec.Release
	}
	return rev, nil
}

// ObjectParts returns the spec and the status of the revision object.
func (r *ReleaseRevision) ObjectParts() (spec, status map[string]any, err error) {
	if spec, err = releaseObjectMap(r.Spec); err != nil {
		return nil, nil, err
	}
	if r.Status != nil {
		if status, err = releaseObjectMap(r.Status); err != nil {
			return nil, nil, err
		}
	}
	return spec, status, nil
}

// WithoutContent returns a copy of the revision without the encoded
// content, for listings.
func (r *ReleaseRevision) WithoutContent() *ReleaseRevision {
	nr := *r
	if r.Spec != nil {
		s := *r.Spec
		s.Content = ""
		nr.Spec = &s
	}
	nr.Resources = nil
	return &nr
}

// ReleaseApplyRequest installs or upgrades a release with the rendered
// resources of a pack.
type ReleaseApplyRequest struct {
	Name        string
	Pack        *ReleasePack
	Description string

	// Values are the variable values of the pack, without secret values,
	// and ValuesHash the hash of all values.
	Values     map[string]any
	ValuesHash string

	// Resources are the rendered resources.
	Resources []*ReleaseResource

	// Secrets are secret Variables items; they are applied but not stored.
	Secrets []*ReleaseVariableSecret

	// Atomic restores the previous state if the operation fails. Unset, the
	// namespace decides (see ReleaseAtomicDefault).
	Atomic *bool

	// Wait waits for the deployments of the jobs, up to Timeout.
	Wait    bool
	Timeout time.Duration

	// TakeOwnership takes over resources owned by other releases.
	TakeOwnership bool

	// MaxHistory is the number of revisions to keep (0: unchanged, or the
	// default for a new release).
	MaxHistory int

	// DryRun plans the operation without changing anything.
	DryRun bool

	// RecordOnly records a revision of resources the client applied itself
	// (client-side apply); nothing is applied and no hooks run.
	RecordOnly bool

	// VariablesSchemas are the variables schemas of the pack and its
	// dependencies. The servers validate VariableValues against them and
	// store them in the revision.
	VariablesSchemas []*ReleaseVariablesSchema

	// VariableValues are the effective variable values (defaults, variable
	// files and overrides) by pack path, for the validation against the
	// variables schemas. They are never stored, as they can contain secret
	// values.
	VariableValues map[string]map[string]any

	// Regions installs the release in several regions in one operation:
	// the receiving region applies it on the leader of every region in turn
	// (the global region first) and reports the result per region.
	Regions []string

	// Multiregion is set by the coordinating region on the request it sends
	// to each region of a multi-region install.
	Multiregion *ReleaseMultiregion

	WriteRequest
}

// ReleaseApplyResponse is the response to an apply, rollback or dry run.
type ReleaseApplyResponse struct {
	Release  *Release
	Revision uint64

	// Changes are the planned (dry run) or performed changes.
	Changes []*ReleaseResourceChange

	Warnings []string

	// Regions are the results per region of a multi-region install, in the
	// order the regions were applied. Release, Revision and Changes are
	// then those of the global region.
	Regions []*ReleaseRegionResult

	WriteMeta
}

// ReleaseRegionResult is the result of a multi-region install in one region.
type ReleaseRegionResult struct {
	Region   string
	Release  *Release
	Revision uint64
	Changes  []*ReleaseResourceChange
	Warnings []string

	// Error is set when the operation failed in the region. The releases of
	// the other regions are kept (partial success).
	Error string
}

// Actions of resource changes.
const (
	ReleaseChangeCreate    = "create"
	ReleaseChangeUpdate    = "update"
	ReleaseChangeDelete    = "delete"
	ReleaseChangeUnchanged = "unchanged"
	ReleaseChangeHook      = "hook"
	ReleaseChangeAdopt     = "adopt"

	// ReleaseChangeRemote marks a resource of a multi-region install that
	// the release of another region applies (Region): global resources and
	// multiregion jobs.
	ReleaseChangeRemote = "remote"
)

// ReleaseResourceChange is a planned or performed change of a resource.
type ReleaseResourceChange struct {
	Type      string
	Namespace string
	Name      string
	Action    string
	Hook      string

	// Owner is the release that owns the resource now ("namespace/name"),
	// if another one.
	Owner string

	// JobDiff is the diff of a job, for dry runs.
	JobDiff *JobDiff

	// Region is the region whose release applies a remote resource.
	Region string

	// Error is set when the change failed.
	Error string
}

// ReleaseRollbackRequest rolls a release back to a revision.
type ReleaseRollbackRequest struct {
	Name string

	// Revision is the revision to restore; 0 selects the revision before
	// the deployed one.
	Revision uint64

	Atomic  *bool
	Wait    bool
	Timeout time.Duration
	DryRun  bool

	WriteRequest
}

// ReleaseDeleteRequest uninstalls a release.
type ReleaseDeleteRequest struct {
	Name string

	// Propagation is "background" (delete the resources, the default) or
	// "orphan" (keep them).
	Propagation string

	// NoHooks skips the pre-delete and post-delete hooks.
	NoHooks bool

	Timeout time.Duration
	WriteRequest
}

// ReleaseDeleteResponse is the response to an uninstall.
type ReleaseDeleteResponse struct {
	Release *Release
	Changes []*ReleaseResourceChange
	WriteMeta
}

// ReleaseTestRequest runs the test hooks of a release.
type ReleaseTestRequest struct {
	Name    string
	Timeout time.Duration
	WriteRequest
}

// ReleaseTestResponse is the response to a test run.
type ReleaseTestResponse struct {
	Result *ReleaseTestResult
	WriteMeta
}

// ReleaseDriftRequest checks a release for drift and updates its Drifted
// condition.
type ReleaseDriftRequest struct {
	Name string
	WriteRequest
}

// ReleaseDriftResponse is the result of a drift check.
type ReleaseDriftResponse struct {
	Drifted   bool
	Revision  uint64
	Resources []*ReleaseDriftEntry
	WriteMeta
}

// ReleaseDriftEntry is a resource whose live state differs from the
// revision.
type ReleaseDriftEntry struct {
	Type      string `json:"type"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`

	// Reason is "missing" or "modified".
	Reason string `json:"reason"`
}

// ReleaseDriftEvent is the payload of ReleaseDrifted events.
type ReleaseDriftEvent struct {
	Namespace string
	Name      string
	Revision  uint64
	Resources []*ReleaseDriftEntry
}

// ReleaseListRequest lists releases in a namespace ("*" for all).
type ReleaseListRequest struct {
	QueryOptions
}

// ReleaseListResponse is the response to a release list request.
type ReleaseListResponse struct {
	Releases []*Release
	QueryMeta
}

// ReleaseSpecificRequest queries a release.
type ReleaseSpecificRequest struct {
	Name string
	QueryOptions
}

// SingleReleaseResponse is the response to a release query.
type SingleReleaseResponse struct {
	Release *Release
	QueryMeta
}

// ReleaseHistoryResponse lists the revisions of a release, newest first,
// without their content.
type ReleaseHistoryResponse struct {
	Revisions []*ReleaseRevision
	QueryMeta
}

// ReleaseRevisionRequest queries a revision with its resources.
type ReleaseRevisionRequest struct {
	Name     string
	Revision uint64
	QueryOptions
}

// SingleReleaseRevisionResponse is the response to a revision query.
type SingleReleaseRevisionResponse struct {
	Revision *ReleaseRevision
	QueryMeta
}

// ReleaseError is a 409 Conflict error of release operations.
func NewReleaseConflictError(format string, args ...any) error {
	return NewErrRPCCoded(409, fmt.Sprintf(format, args...))
}

// ReleaseErrors joins errors of several resources.
func ReleaseErrors(errs []error) error {
	var mErr *multierror.Error
	for _, err := range errs {
		mErr = multierror.Append(mErr, err)
	}
	return mErr.ErrorOrNil()
}
