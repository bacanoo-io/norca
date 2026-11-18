// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host

import (
	"context"
	"time"

	"github.com/bacanoo-io/norca-rig/module"
	"github.com/bacanoo-io/norca-rig/resource"
)

// ReleaseRef names a release. Releases live in a namespace.
type ReleaseRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ModuleRef identifies the module version a revision was made from.
type ModuleRef struct {
	// Name is the name of the module (its module block).
	Name string `json:"name"`
	// Source is the address of the module, for example "norca/nats".
	Source string `json:"source"`
	// Version is the exact version.
	Version string `json:"version"`
	// Ref is the tag the version was found by; empty for a local module.
	Ref string `json:"ref,omitempty"`
	// Digest is the tree digest of the module version (design 8.1).
	Digest string `json:"digest"`
	// Signers are the key IDs of the signatures that verified the digest.
	Signers []string `json:"signers,omitempty"`
	// Requires are the requirements of the module on the cluster, which
	// the server checks.
	Requires *module.Requires `json:"requires,omitempty"`
	// Signatures are the signatures of the digest, which the admission of
	// the server verifies against its trusted keys (design 8.2).
	Signatures []Signature `json:"signatures,omitempty"`
}

// Signature is a signed statement about a tree digest (design 8.1): a
// cosign simple signing payload and its signature.
type Signature struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Revision is a revision of a release, as the engine needs it.
type Revision struct {
	Release ReleaseRef `json:"release"`
	// Number is the number of the revision, counted by the server.
	Number uint64 `json:"number"`
	// Module is the module version the revision was made from.
	Module ModuleRef `json:"module"`
	// Digest covers the module digest, the values and the settings the
	// revision was applied with. The engine compares it with its own to
	// decide that a release is unchanged (design 9.2, 9.3).
	Digest string `json:"digest"`
	// Owner is the name of the stack that installed the release; empty for
	// a release installed on its own.
	Owner string `json:"owner,omitempty"`
	// Status is the status of the revision as the server reports it.
	Status string `json:"status"`
	// CreatedAt is when the server made the revision.
	CreatedAt time.Time `json:"created_at"`
}

// Settings are the settings of a release that a stack may set (design 6.4).
type Settings struct {
	// Atomic restores the previous revision when an apply fails; nil lets
	// the namespace decide.
	Atomic *bool `json:"atomic,omitempty"`
	// Wait waits until the deployments of the jobs are healthy.
	Wait bool `json:"wait"`
	// Timeout bounds the wait; zero leaves it to the server.
	Timeout time.Duration `json:"timeout,omitempty"`
	// MaxHistory is the number of revisions the server keeps; zero leaves
	// it to the server.
	MaxHistory int `json:"max_history,omitempty"`
}

// ApplyRequest is what the engine asks the release API to plan or apply.
type ApplyRequest struct {
	Release ReleaseRef `json:"release"`
	Module  ModuleRef  `json:"module"`
	// Digest is recorded in the revision; see Revision.Digest.
	Digest string `json:"digest"`
	// Owner is the name of the stack; see Revision.Owner.
	Owner string `json:"owner,omitempty"`
	// Values are the values of the variables of the module, without the
	// sensitive ones: they are recorded in the revision (design 6.3).
	Values map[string]any `json:"values,omitempty"`
	// Resources are the evaluated resources of the release.
	Resources []resource.Resource `json:"resources"`
	// Secrets are the secret items of the Variable resources, by the name
	// of the resource: never recorded in a revision (design 6.3).
	Secrets  map[string]map[string]Sensitive `json:"-"`
	Settings Settings                        `json:"settings"`
}

// Action is what a plan does to a resource.
type Action string

// The actions of a plan.
const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// Change is one change of a plan.
type Change struct {
	Kind   resource.Kind `json:"kind"`
	Name   string        `json:"name"`
	Action Action        `json:"action"`
}

// Plan is the server's answer to what an apply would change.
type Plan struct {
	Release ReleaseRef `json:"release"`
	// Changes are the changes per resource; none when nothing differs.
	Changes []Change `json:"changes"`
}

// DriftEntry is a resource of a release that differs in the cluster from
// what the current revision applied.
type DriftEntry struct {
	Kind resource.Kind `json:"kind"`
	Name string        `json:"name"`
	// Detail says what differs, in the words of the server.
	Detail string `json:"detail"`
}

// Drift is the result of the server's drift check of a release.
type Drift struct {
	Release ReleaseRef   `json:"release"`
	Entries []DriftEntry `json:"entries"`
}

// ReleaseReader reads the revisions of releases.
type ReleaseReader interface {
	// Current returns the current revision of a release, or an error
	// matching ErrNotFound when the release does not exist.
	Current(ctx context.Context, ref ReleaseRef) (Revision, error)
	// History returns the revisions of a release, newest first.
	History(ctx context.Context, ref ReleaseRef) ([]Revision, error)
	// Owned returns the current revisions of the releases a stack owns,
	// for prune (design 9.3).
	Owned(ctx context.Context, owner string) ([]Revision, error)
}

// ReleasePlanner compares what a release should be with what the cluster
// holds.
type ReleasePlanner interface {
	// Plan returns what applying the request would change. It changes
	// nothing.
	Plan(ctx context.Context, req ApplyRequest) (Plan, error)
	// Drift returns the resources of a release that differ in the cluster
	// from its current revision.
	Drift(ctx context.Context, ref ReleaseRef) (Drift, error)
}

// ReleaseWriter changes releases. Each call runs under the server's lock of
// the release.
type ReleaseWriter interface {
	// Apply applies the request as a new revision and waits as its settings
	// say.
	Apply(ctx context.Context, req ApplyRequest) (Revision, error)
	// Rollback makes an earlier revision current again, as a new revision.
	Rollback(ctx context.Context, ref ReleaseRef, number uint64) (Revision, error)
	// Uninstall removes a release and its resources.
	Uninstall(ctx context.Context, ref ReleaseRef) error
}

// Releases is the release API of the cluster: the releases of today's
// server, used as they are (design 2.1, 9.1).
type Releases interface {
	ReleaseReader
	ReleasePlanner
	ReleaseWriter
}
