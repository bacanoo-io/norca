// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/syntax"
	"github.com/bacanoo-io/norca/rigger/resource"
)

// textBlocks name, by kind, the attribute that holds a document in HCL of
// the host and may be written as a block: the rules of an ACL policy. The
// block is written in HCL as it evaluates (renderBody: its order, dynamic
// blocks expanded, nulls left out), and the text is the attribute. The
// module keeps the highlighting, the checks and the dynamic blocks of HCL
// where a heredoc would be a string.
var textBlocks = map[resource.Kind]string{
	resource.KindACLPolicy: "rules",
}

// textBlock replaces the block name of v, the value of body, with its text.
// A body with the attribute of that name is left as it is; both is an
// error.
func textBlock(body hcl.Body, name string, v cty.Value, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	native, ok := body.(*hclsyntax.Body)
	if !ok || v.IsNull() || !v.Type().IsObjectType() {
		return v, nil
	}

	var blocks []*hclsyntax.Block

	for _, b := range native.Blocks {
		if b.Type == name {
			blocks = append(blocks, b)
		}
	}

	if len(blocks) == 0 {
		return v, nil
	}

	if a, ok := native.Attributes[name]; ok || len(blocks) > 1 || len(blocks[0].Labels) > 0 {
		rng := blocks[0].DefRange()
		if ok {
			rng = a.SrcRange
		}

		return v, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Invalid " + name,
			Detail:  name + " is one attribute holding the text, or one block without labels holding the document.",
			Subject: rng.Ptr(),
		}}
	}

	f := hclwrite.NewEmptyFile()
	if diags := renderBody(f.Body(), blocks[0].Body, ctx, nil, nil); diags.HasErrors() {
		return v, diags
	}

	// The host parses the text again, as deep as the values in it.
	if _, over := syntax.ParseConfig(f.Bytes(), name); over.HasErrors() {
		return v, over
	}

	attrs := v.AsValueMap()
	attrs[name] = cty.StringVal(string(hclwrite.Format(f.Bytes())))

	return cty.ObjectVal(attrs), nil
}
