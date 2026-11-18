// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"github.com/hashicorp/hcl/v2"
)

// withoutNulls is a body whose attributes with a null value are left out,
// at every depth: `to = local.bridge ? var.port : null` writes no `to`
// (design 4.1). A decoder of the host sees the attribute as missing, so a
// typed field keeps its zero value rather than failing on null (or, for a
// duration of Norca's job decoder, panicking).
//
// An attribute whose value can't be evaluated here (one that reads the
// iterator of a dynamic block, which the expansion adds later) is kept for
// the decoder to evaluate.
type withoutNulls struct {
	body hcl.Body
	ctx  *hcl.EvalContext
}

func dropNulls(body hcl.Body, ctx *hcl.EvalContext) hcl.Body {
	return &withoutNulls{body: body, ctx: ctx}
}

func (b *withoutNulls) Content(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Diagnostics) {
	content, diags := b.body.Content(schema)

	return b.filter(content), diags
}

func (b *withoutNulls) PartialContent(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Body, hcl.Diagnostics) {
	content, rest, diags := b.body.PartialContent(schema)

	return b.filter(content), dropNulls(rest, b.ctx), diags
}

func (b *withoutNulls) JustAttributes() (hcl.Attributes, hcl.Diagnostics) {
	attrs, diags := b.body.JustAttributes()

	return b.attributes(attrs), diags
}

func (b *withoutNulls) MissingItemRange() hcl.Range { return b.body.MissingItemRange() }

func (b *withoutNulls) filter(content *hcl.BodyContent) *hcl.BodyContent {
	if content == nil {
		return nil
	}

	out := *content
	out.Attributes = b.attributes(content.Attributes)

	out.Blocks = make(hcl.Blocks, len(content.Blocks))
	for i, block := range content.Blocks {
		nb := *block
		nb.Body = dropNulls(block.Body, b.ctx)
		out.Blocks[i] = &nb
	}

	return &out
}

func (b *withoutNulls) attributes(attrs hcl.Attributes) hcl.Attributes {
	if attrs == nil {
		return nil
	}

	out := make(hcl.Attributes, len(attrs))

	for name, a := range attrs {
		v, diags := a.Expr.Value(b.ctx)
		if !diags.HasErrors() && v.IsNull() {
			continue
		}

		out[name] = a
	}

	return out
}
