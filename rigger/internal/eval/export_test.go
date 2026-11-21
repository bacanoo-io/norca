// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"context"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/host"
)

// genericJobs decodes a job as the value of its block, with no schema: the
// tests of the core have no decoder of Norca.
type genericJobs struct{}

// GenericJobs is a job decoder for the tests.
var GenericJobs host.JobDecoder = genericJobs{}

func (genericJobs) DecodeJob(_ context.Context, spec host.JobSpec) (host.Job, hcl.Diagnostics) {
	w := &walker{skip: metaNames}

	v, diags := w.value(spec.Block.Body, spec.EvalContext)
	if diags.HasErrors() {
		return host.Job{}, diags
	}

	data, err := marshal(v)
	if err != nil {
		return host.Job{}, hcl.Diagnostics{valueDiag("job", err, spec.Block.DefRange)}
	}

	return host.Job{Spec: data}, nil
}
