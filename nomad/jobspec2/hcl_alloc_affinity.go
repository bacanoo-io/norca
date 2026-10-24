// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/nomad/api"
)

// allocAffinityHCL has the fields and HCL tags of api.AllocAffinity but no
// registered block decoder, so the default decoder handles it.
type allocAffinityHCL api.AllocAffinity

// decodeAllocAffinity decodes a colocate or separate block. Like meta, the
// selector accepts both block syntax (`selector { app = "api" }`) and
// attribute syntax (`selector = { "app.kubernetes.io/name" = "api" }`), the
// latter allowing keys that are not valid HCL identifiers.
func decodeAllocAffinity(body hcl.Body, ctx *hcl.EvalContext, val interface{}) hcl.Diagnostics {
	a := val.(*api.AllocAffinity)

	selectorAttr, body, diags := decodeAsAttribute(body, ctx, "selector")
	if diags.HasErrors() {
		return diags
	}

	var decoded allocAffinityHCL
	diags = append(diags, hclDecoder.DecodeBody(body, ctx, &decoded)...)
	*a = api.AllocAffinity(decoded)

	if selectorAttr != nil {
		a.Selector = selectorAttr
	}
	return diags
}
