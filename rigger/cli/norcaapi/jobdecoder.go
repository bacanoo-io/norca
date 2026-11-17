// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca-rig/cli/internal/jobspec2"
	"github.com/bacanoo-io/norca-rig/host"
)

// JobDecoder decodes job blocks with the job decoder of Norca. The job it
// returns is the JSON of Norca's API job, which the release API takes.
//
// The zero value is ready to use.
type JobDecoder struct{}

var _ host.JobDecoder = JobDecoder{}

// DecodeJob decodes the job block of spec. Errors are diagnostics with the
// position of the block or attribute they concern; a cancelled context is
// one too.
func (JobDecoder) DecodeJob(ctx context.Context, spec host.JobSpec) (host.Job, hcl.Diagnostics) {
	if spec.Block == nil {
		return host.Job{}, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "No job block",
			Detail:   "The job decoder was given no block.",
		}}
	}

	if err := ctx.Err(); err != nil {
		return host.Job{}, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Job not decoded",
			Detail:   "The decoding was cancelled: " + err.Error() + ".",
			Subject:  spec.Block.DefRange.Ptr(),
		}}
	}

	job, diags := jobspec2.DecodeJob(spec.Block, spec.EvalContext)
	if diags.HasErrors() {
		return host.Job{}, diags
	}

	data, err := json.Marshal(job)
	if err != nil {
		return host.Job{}, append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Job not encoded",
			Detail:   "The decoded job could not be encoded as JSON: " + err.Error() + ".",
			Subject:  spec.Block.DefRange.Ptr(),
		})
	}

	decoded := host.Job{Spec: data}
	if job.ID != nil {
		decoded.ID = *job.ID
	}

	if job.Namespace != nil {
		decoded.Namespace = *job.Namespace
	}

	return decoded, diags
}
