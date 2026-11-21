// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/resource"
)

// JobSpec is a job block of a module, ready to be decoded.
//
// Block is the block as parsed, without the meta-arguments of Rigger (enabled,
// for_each, count); its label is the name the module refers to the job by,
// the ID when the block has no id attribute (design 4.5), and its ranges
// place the diagnostics. EvalContext holds the install-time values
// (var.*, local.*, module.*, rigger.*) and the functions of the engine. A
// reference the context does not hold, such as a node attribute or a task
// variable, is a runtime reference: the context gives it back as its text
// (hcl.EvalContext.UndefinedVariable), as the job decoder of Norca does for
// a job file today (design 4.2).
//
// The decoder of Norca takes such a context through a small exported entry
// point; until norca has it, the rigger program carries it in a copy of the
// decoder (design, question 4).
type JobSpec struct {
	// Block is the job block.
	Block *hcl.Block
	// EvalContext resolves the install-time references of the block.
	EvalContext *hcl.EvalContext
	// Hook is the hook block of the job, which Rigger took off the block; nil
	// for a job that is not a hook. The host writes it the way its release
	// API reads hooks.
	Hook *resource.Hook
}

// Job is a decoded job in the encoding of the host: for Norca, the JSON of
// its API job. The engine does not look into Spec; it stores it in a
// resource and hands it to the release API.
type Job struct {
	// ID is the ID of the job.
	ID string `json:"id"`
	// Namespace is the namespace of the job; empty means the namespace of
	// the release.
	Namespace string `json:"namespace,omitempty"`
	// Spec is the job in the encoding of the host.
	Spec json.RawMessage `json:"spec"`
}

// JobDecoder turns a job block into a job the cluster accepts.
//
// The job specification belongs to Norca, so its decoder is the host's: the
// rigger program links the decoder of Norca, the server uses its own, and the
// engine never follows the job schema by hand (design 12.2).
type JobDecoder interface {
	// DecodeJob decodes the block. Errors are diagnostics with the position
	// of the block or attribute they concern.
	DecodeJob(ctx context.Context, spec JobSpec) (Job, hcl.Diagnostics)
}

// ResourceDecoder decodes the resources of the kinds whose specification
// has a file format of the host's own, where the order of blocks counts
// (the columns of an object kind) and blocks are named as that format names
// them. The engine gives the block of the resource as it evaluated it,
// written as HCL in the order of the module, without its meta-arguments and
// with its dynamic blocks expanded; the decoder returns the resource in its
// encoding. Kinds the decoder does not name are the value of their block.
type ResourceDecoder interface {
	// Kinds are the kinds the decoder decodes.
	Kinds() []resource.Kind
	// DecodeResource decodes the block of the resource name of kind, written
	// in src.
	DecodeResource(ctx context.Context, kind resource.Kind, name string, src []byte) (json.RawMessage, hcl.Diagnostics)
}
