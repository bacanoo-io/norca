// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/dynblock"
	"github.com/zclconf/go-cty/cty"
)

// withoutNulls is the body of a job as the decoder of the host is given it:
//
//   - Its attributes with a null value are left out, at every depth: `to =
//     local.bridge ? var.port : null` writes no `to` (design 4.1). The
//     decoder sees the attribute as missing, so a typed field keeps its zero
//     value rather than failing on null (or, for a duration of Norca's job
//     decoder, panicking).
//   - An attribute whose value is an object, where the decoder takes a block
//     of that name without labels and no attribute, is that block: `env =
//     { "DB_${i}_PASSWORD" = "password" }` in a secret block, whose env is a
//     block of names the module computes.
//
// The dynamic blocks are expanded before (jobBody), so an attribute of their
// content is evaluated with its iterator. An attribute whose value can't be
// evaluated here is kept for the decoder to report.
type withoutNulls struct {
	body hcl.Body
	ctx  *hcl.EvalContext
	// source is the body as written, before the dynamic blocks were
	// expanded, for the walk of the body; nil below the top.
	source hcl.Body
}

// jobBody is the body of a job for the decoder: read as Norca's job decoder
// reads a job file, with the blocks of its free-form parts as values
// (freeForm), its dynamic blocks expanded, so that every attribute is
// evaluated with the iterators of its blocks, then without its nulls.
func jobBody(body hcl.Body, ctx *hcl.EvalContext) hcl.Body {
	return &withoutNulls{body: dynblock.Expand(asFreeForm(body), ctx), ctx: ctx, source: body}
}

func dropNulls(body hcl.Body, ctx *hcl.EvalContext) hcl.Body {
	return &withoutNulls{body: body, ctx: ctx}
}

func (b *withoutNulls) Content(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Diagnostics) {
	wide, asBlocks := b.widen(schema)

	content, diags := b.body.Content(wide)

	return b.blocksOf(b.filter(content), asBlocks), diags
}

func (b *withoutNulls) PartialContent(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Body, hcl.Diagnostics) {
	wide, asBlocks := b.widen(schema)

	content, rest, diags := b.body.PartialContent(wide)

	return b.blocksOf(b.filter(content), asBlocks), dropNulls(rest, b.ctx), diags
}

// widen adds to schema an attribute for each block type without labels that
// the body writes as an attribute with an object value, and names them.
func (b *withoutNulls) widen(schema *hcl.BodySchema) (*hcl.BodySchema, map[string]bool) {
	attrs, _ := b.body.JustAttributes() // an hclsyntax body gives every attribute, blocks or not

	var asBlocks map[string]bool

	for _, bs := range schema.Blocks {
		if len(bs.LabelNames) > 0 || hasAttribute(schema, bs.Type) {
			continue
		}

		a, ok := attrs[bs.Type]
		if !ok {
			continue
		}

		v, diags := objectValue(a, b.ctx)
		if diags.HasErrors() || v.IsNull() || !v.IsWhollyKnown() || (!v.Type().IsObjectType() && !v.Type().IsMapType()) {
			continue
		}

		if asBlocks == nil {
			asBlocks = map[string]bool{}
		}

		asBlocks[bs.Type] = true
	}

	if asBlocks == nil {
		return schema, nil
	}

	wide := *schema
	wide.Attributes = slices.Clone(schema.Attributes)

	for name := range asBlocks {
		wide.Attributes = append(wide.Attributes, hcl.AttributeSchema{Name: name})
	}

	return &wide, asBlocks
}

func hasAttribute(schema *hcl.BodySchema, name string) bool {
	return slices.ContainsFunc(schema.Attributes, func(a hcl.AttributeSchema) bool { return a.Name == name })
}

// blocksOf turns the attributes named in asBlocks into blocks of the same
// name.
func (b *withoutNulls) blocksOf(content *hcl.BodyContent, asBlocks map[string]bool) *hcl.BodyContent {
	if content == nil || asBlocks == nil {
		return content
	}

	for name := range asBlocks {
		a, ok := content.Attributes[name]
		if !ok {
			continue
		}

		delete(content.Attributes, name)

		v, _ := objectValue(a, b.ctx)

		body := &mapBody{attrs: hcl.Attributes{}, rng: a.Range}

		for it := v.ElementIterator(); it.Next(); {
			k, ev := it.Element()
			if ev.IsNull() {
				continue
			}

			body.attrs[k.AsString()] = &hcl.Attribute{
				Name: k.AsString(), Expr: hcl.StaticExpr(ev, a.Expr.Range()), Range: a.Range, NameRange: a.NameRange,
			}
		}

		content.Blocks = append(content.Blocks, &hcl.Block{
			Type: name, Body: body, DefRange: a.NameRange, TypeRange: a.NameRange,
		})
	}

	return content
}

// mapBody is the body of a block made from an object: its attributes only.
type mapBody struct {
	attrs hcl.Attributes
	rng   hcl.Range
}

func (m *mapBody) Content(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Diagnostics) {
	content, rest, diags := m.partial(schema)

	for name, a := range rest.attrs {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Unsupported argument",
			Detail: fmt.Sprintf("An argument named %q is not expected here.", name), Subject: a.NameRange.Ptr(),
		})
	}

	return content, diags
}

func (m *mapBody) PartialContent(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Body, hcl.Diagnostics) {
	return m.partial(schema)
}

func (m *mapBody) partial(schema *hcl.BodySchema) (*hcl.BodyContent, *mapBody, hcl.Diagnostics) {
	content := &hcl.BodyContent{Attributes: hcl.Attributes{}, MissingItemRange: m.rng}
	rest := &mapBody{attrs: hcl.Attributes{}, rng: m.rng}

	var diags hcl.Diagnostics

	for name, a := range m.attrs {
		if hasAttribute(schema, name) {
			content.Attributes[name] = a
		} else {
			rest.attrs[name] = a
		}
	}

	for _, as := range schema.Attributes {
		if _, ok := m.attrs[as.Name]; as.Required && !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Missing required argument",
				Detail: fmt.Sprintf("The argument %q is required.", as.Name), Subject: m.rng.Ptr(),
			})
		}
	}

	return content, rest, diags
}

func (m *mapBody) JustAttributes() (hcl.Attributes, hcl.Diagnostics) { return m.attrs, nil }

func (m *mapBody) MissingItemRange() hcl.Range { return m.rng }

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

// objectValue is the value of an attribute that may stand for a block: the
// object it is written as. A free-form body (freeForm) gives an attribute
// written as an object constructor as a tuple of that object, as the job
// decoder of Norca reads one; for a block the decoder takes by its schema
// the object itself is meant.
func objectValue(a *hcl.Attribute, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	v, diags := a.Expr.Value(ctx)

	wrapped := hcl.UnwrapExpressionUntil(a.Expr, func(e hcl.Expression) bool {
		_, ok := e.(*tupleOfOne)

		return ok
	})
	if _, ok := wrapped.(*tupleOfOne); ok && !diags.HasErrors() && v.IsKnown() && !v.IsNull() &&
		v.Type().IsTupleType() && v.LengthInt() == 1 {
		return v.Index(cty.NumberIntVal(0)), diags
	}

	return v, diags
}
