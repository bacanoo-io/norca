// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package resource holds the evaluated resources of a module.
//
// They are plain data that serializes, so that a server can pass them
// through its RPC and keep them in a revision (design 12.3). What a resource
// holds is in the encoding the host uses for its kind; the engine does not
// look into it.
package resource

import (
	"encoding/json"
	"slices"
)

// Kind is the kind of resource. The kinds of Norca's releases are built
// in, under the names the release API uses for them; more kinds come by
// registration (design 5, 11).
type Kind string

// The built-in kinds. The file of a resource is named by its kind
// (design 4.5): <name>.namespace.hcl, <name>.quota.hcl, <name>.acl.hcl,
// <name>.var.hcl, <name>.kind.hcl, <name>.admission.hcl,
// <name>.external.hcl, <name>.object.hcl and <name>.norca.hcl.
const (
	KindNamespace       Kind = "namespace"
	KindQuota           Kind = "quota"
	KindACLPolicy       Kind = "acl-policy"
	KindVariable        Kind = "variable"
	KindObjectKind      Kind = "object-kind"
	KindAdmissionPolicy Kind = "admission-policy"
	KindExternalService Kind = "external-service"
	KindObject          Kind = "object"
	KindJob             Kind = "job"
)

// builtin is the order in which the server applies the kinds of a release
// (releaseResourceOrder of norca). A quota comes before the namespaces: a
// namespace is written with the quota it names, and is deleted before it.
var builtin = []Kind{
	KindQuota,
	KindNamespace,
	KindACLPolicy,
	KindVariable,
	KindObjectKind,
	KindAdmissionPolicy,
	KindExternalService,
	KindObject,
	KindJob,
}

// Builtin returns the built-in kinds in the order the server applies them.
// The slice is a copy; the caller may change it.
func Builtin() []Kind {
	return slices.Clone(builtin)
}

// Resource is one evaluated resource of a release.
type Resource struct {
	// Kind is the kind of the resource.
	Kind Kind `json:"kind"`
	// Name is the label of its block: the name the module refers to it by.
	Name string `json:"name"`
	// Spec is the resource in the encoding the host uses for its kind.
	Spec json.RawMessage `json:"spec"`
	// Hook makes a job a hook of the release; nil for a resource that is
	// not one (design 5).
	Hook *Hook `json:"hook,omitempty"`
}

// Hook is the hook block of a job: when the release runs it, in which
// order, how long it may take and when it is deleted.
type Hook struct {
	// On are the phases: pre-install, post-install, pre-upgrade,
	// post-upgrade, pre-delete, post-delete, test.
	On []string `json:"on"`
	// Weight orders the hooks of a phase, the lowest first.
	Weight int `json:"weight,omitempty"`
	// Timeout is how long the hook may run, as a duration ("5m"); empty
	// is the server's default.
	Timeout string `json:"timeout,omitempty"`
	// Delete is when the job is deleted after it ran: on-success, always
	// or never; empty is the server's default.
	Delete string `json:"delete,omitempty"`
}

// HookPhases are the phases a hook may run on.
func HookPhases() []string {
	return []string{"pre-install", "post-install", "pre-upgrade", "post-upgrade", "pre-delete", "post-delete", "test"}
}

// HookDeletes are the values of Hook.Delete.
func HookDeletes() []string {
	return []string{"on-success", "always", "never"}
}
