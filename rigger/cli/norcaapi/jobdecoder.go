// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/jobspec2"
	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/resource"
	"github.com/bacanoo-io/norca/rigger/runref"
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

	if spec.Hook != nil {
		writeHook(job, spec.Hook)
	}

	data, err := encodeJob(job)
	if err != nil {
		return host.Job{}, append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Job not encoded",
			Detail:   "The decoded job could not be encoded: " + err.Error() + ".",
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

// encodeJob resolves the runtime references of a job (package runref) and
// encodes it as JSON. In the data of a task template a reference is written
// for the template ({{ env "X" }}, a lookup of a secret); everywhere else as
// ${X}, which the client interpolates, and a reference only a template can
// resolve is an error.
func encodeJob(job *api.Job) ([]byte, error) {
	for _, tg := range job.TaskGroups {
		for _, task := range tg.Tasks {
			for _, tmpl := range task.Templates {
				if tmpl.EmbeddedTmpl == nil {
					continue
				}

				data, err := runref.Replace(*tmpl.EmbeddedTmpl, func(p runref.Part) (string, error) {
					return p.Resolve(runref.PlaceTemplate)
				})
				if err != nil {
					return nil, fmt.Errorf("template of task %s: %w", task.Name, err)
				}

				tmpl.EmbeddedTmpl = &data
			}
		}
	}

	data, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}

	// What is left is outside the templates. A part has no character JSON
	// escapes, so it is replaced in the encoded text, by the JSON of what
	// it resolves to.
	text, err := runref.Replace(string(data), func(p runref.Part) (string, error) {
		s, err := p.Resolve(runref.PlaceJob)
		if err != nil {
			return "", err
		}

		quoted, err := json.Marshal(s)
		if err != nil {
			return "", err
		}

		return string(quoted[1 : len(quoted)-1]), nil
	})
	if err != nil {
		return nil, err
	}

	return []byte(text), nil
}

// writeHook writes a hook into the meta of a job, where the release API of
// Norca reads it.
func writeHook(job *api.Job, h *resource.Hook) {
	if job.Meta == nil {
		job.Meta = map[string]string{}
	}

	job.Meta[api.ReleaseMetaHook] = strings.Join(h.On, ",")
	job.Meta[api.ReleaseMetaHookWeight] = strconv.Itoa(h.Weight)

	if h.Timeout != "" {
		job.Meta[api.ReleaseMetaHookTimeout] = h.Timeout
	}

	if h.Delete != "" {
		job.Meta[api.ReleaseMetaHookDelete] = h.Delete
	}
}
