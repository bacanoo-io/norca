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

// Kind is the kind of a resource. The kinds of Norca's releases are built
// in; more kinds come by registration (design 5, 11).
type Kind string

// The built-in kinds. The file of a resource is named by its kind
// (design 4.5): <name>.namespace.hcl, <name>.quota.hcl, <name>.acl.hcl,
// <name>.var.hcl, <name>.kind.hcl, <name>.admission.hcl,
// <name>.external.hcl, <name>.object.hcl and <name>.norca.hcl.
const (
	KindNamespace       Kind = "namespace"
	KindQuota           Kind = "quota"
	KindACLPolicy       Kind = "acl_policy"
	KindVariable        Kind = "variable"
	KindObjectKind      Kind = "object_kind"
	KindAdmissionPolicy Kind = "admission_policy"
	KindExternalService Kind = "external_service"
	KindObject          Kind = "object"
	KindJob             Kind = "job"
)

// builtin is the order in which the server applies the kinds of a release.
var builtin = []Kind{
	KindNamespace,
	KindQuota,
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
}
