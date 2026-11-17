// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/hcl/v2"
)

// JobSpec is a job block of a module, ready to be decoded.
//
// Body is the block as parsed. EvalContext holds the install-time values
// (var.*, local.*, module.*, rig.*) and the functions of the engine. A
// reference the context does not hold, such as a node attribute or a task
// variable, is a runtime reference: the decoder passes it through untouched,
// as the job decoder of Norca does today (design 4.2).
//
// Whether the decoder of Norca can take these values from outside as it is,
// or needs a small change, is the spike of phase 0 (design, question 4).
type JobSpec struct {
	// Filename is the file of the block, for diagnostics.
	Filename string
	// Body is the content of the job block.
	Body hcl.Body
	// EvalContext resolves the install-time references of the block.
	EvalContext *hcl.EvalContext
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
// rig program links the decoder of Norca, the server uses its own, and the
// engine never follows the job schema by hand (design 12.2).
type JobDecoder interface {
	// DecodeJob decodes the block. Errors are diagnostics with the position
	// of the block or attribute they concern.
	DecodeJob(ctx context.Context, spec JobSpec) (Job, hcl.Diagnostics)
}
