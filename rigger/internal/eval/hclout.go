// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// renderBlock writes a block as it evaluates, as HCL for a decoder of the
// host (host.ResourceDecoder): its attributes as values in the order of the
// module (a null one left out), its blocks in their order, dynamic blocks
// expanded, without the names of skip at the top. A runtime reference is
// written as outside a task template, ${...}. A sensitive value is refused:
// the text goes to the host.
func renderBlock(block *hcl.Block, label string, ctx *hcl.EvalContext, skip map[string]bool, x *expansion) ([]byte, hcl.Diagnostics) {
	f := hclwrite.NewEmptyFile()

	out := f.Body().AppendNewBlock(block.Type, []string{label})

	if diags := renderBody(out.Body(), block.Body, ctx, skip, x); diags.HasErrors() {
		return nil, diags
	}

	return f.Bytes(), nil
}

func renderBody(out *hclwrite.Body, body hcl.Body, ctx *hcl.EvalContext, skip map[string]bool, x *expansion) hcl.Diagnostics {
	syntax, ok := body.(*hclsyntax.Body)
	if !ok {
		return unsupportedSyntax()
	}

	attrs := make([]*hclsyntax.Attribute, 0, len(syntax.Attributes))

	for name, a := range syntax.Attributes {
		if !skip[name] {
			attrs = append(attrs, a)
		}
	}

	slices.SortFunc(attrs, func(a, b *hclsyntax.Attribute) int { return a.SrcRange.Start.Byte - b.SrcRange.Start.Byte })

	var diags hcl.Diagnostics

	for _, a := range attrs {
		v, moreDiags := a.Expr.Value(ctx)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() || v.IsNull() {
			continue
		}

		if v.ContainsMarked() {
			diags = append(diags, sensitiveDiag("this resource", a.Expr.Range()))

			continue
		}

		if !v.IsWhollyKnown() {
			diags = append(diags, unknownDiag(a.Name, a.Expr.Range()))

			continue
		}

		// The reader of the text does not know a runtime reference as it is
		// inside the engine: it is resolved as in the value of a block the
		// engine encodes itself (marshal).
		v, err := resolveRuntime(v)
		if err != nil {
			diags = append(diags, valueDiag(a.Name, err, a.Expr.Range()))

			continue
		}

		out.SetAttributeValue(a.Name, v)
	}

	for _, b := range syntax.Blocks {
		if b.Type == "dynamic" {
			diags = append(diags, renderDynamic(out, b, ctx, x)...)

			continue
		}

		nb := out.AppendNewBlock(b.Type, b.Labels)
		diags = append(diags, renderBody(nb.Body(), b.Body, ctx, nil, x)...)
	}

	return diags
}

// renderDynamic writes the blocks a dynamic block makes.
func renderDynamic(out *hclwrite.Body, b *hclsyntax.Block, ctx *hcl.EvalContext, x *expansion) hcl.Diagnostics {
	typ, content, items, diags := expandDynamic(b, ctx, x)
	if diags.HasErrors() {
		return diags
	}

	for _, item := range items {
		nb := out.AppendNewBlock(typ, item.labels)
		diags = append(diags, renderBody(nb.Body(), content, item.ctx, nil, x)...)

		if x.exceeded().HasErrors() {
			break
		}
	}

	return diags
}
