// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"github.com/bacanoo-io/norca/norca/api"
	"github.com/hashicorp/hcl/v2"
)

// DecodeJob is the entry point the spike of Rig tries out: it decodes one
// job block with an evaluation context given from outside, the way
// ParseWithConfig decodes one with its own.
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
