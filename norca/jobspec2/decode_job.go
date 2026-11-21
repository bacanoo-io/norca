// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"github.com/bacanoo-io/norca/norca/api"
	"github.com/hashicorp/hcl/v2"
)

// DecodeJob decodes one job block with an evaluation context given from
// outside, the way ParseWithConfig decodes a job file with its own: dynamic
// blocks, meta, tasks and vault, and the normalization of the job.
//
// Rigger decodes the job blocks of its modules with it: the context holds
// the install-time values of Rigger (var.*, local.*, module.*, rigger.*), its
// functions, and an UndefinedVariable callback that gives a runtime
// reference back as its text. The label of the block is the ID of the job.
func DecodeJob(block *hcl.Block, ctx *hcl.EvalContext) (*api.Job, hcl.Diagnostics) {
	c := newJobConfig(&ParseConfig{})

	diags := c.decodeJob(&hcl.BodyContent{Blocks: hcl.Blocks{block}}, ctx)
	if diags.HasErrors() {
		return nil, diags
	}

	diags = append(diags, decodeMapInterfaceType(&c.Job, ctx)...)
	diags = append(diags, decodeMapInterfaceType(&c.Tasks, ctx)...)
	diags = append(diags, decodeMapInterfaceType(&c.Vault, ctx)...)

	if diags.HasErrors() {
		return nil, diags
	}

	normalizeJob(c)

	return c.Job, diags
}
