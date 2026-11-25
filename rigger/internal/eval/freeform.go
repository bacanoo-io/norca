// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// freeForm is the body of a job as Norca's job decoder reads one: where the
// decoder has no schema for a block (the config of a task driver, the policy
// of a scaling block) it takes the body as attributes, and a nested block
// there is a value as well. The decoder does that itself for a body it
// parses (jobspec2 wraps the job with hclutil.BlocksAsAttrs), but only for a
// body of the HCL syntax package, and the body of a job of a module reaches
// it wrapped (jobBody). So the body is read the same way here, before the
// wrapping:
//
//   - the blocks of one type are a tuple of objects, in their order;
//   - a block with labels is an object keyed by its first label, whose value
//     is a tuple of the block with the other labels: `check "cpu" { }` is
//     {cpu = [{...}]};
//   - an attribute written as an object constructor is a tuple of that one
//     object, as a block of the same name would be;
//   - an attribute or a block the decoder took by its schema is not one of
//     the attributes.
type freeForm struct {
	body *hclsyntax.Body
	// taken are the names of the attributes and the types of the blocks the
	// decoder took from this body by a schema (PartialContent).
	taken map[string]bool
}

// asFreeForm wraps a body of the HCL syntax package; any other body is left
// as it is.
func asFreeForm(body hcl.Body) hcl.Body {
	if sb, ok := body.(*hclsyntax.Body); ok {
		return &freeForm{body: sb}
	}

	return body
}

func (f *freeForm) Content(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Diagnostics) {
	content, diags := f.body.Content(schema)

	return wrapBlocks(content), diags
}

func (f *freeForm) PartialContent(schema *hcl.BodySchema) (*hcl.BodyContent, hcl.Body, hcl.Diagnostics) {
	content, rest, diags := f.body.PartialContent(schema)

	taken := make(map[string]bool, len(f.taken)+len(schema.Attributes)+len(schema.Blocks))
	for name := range f.taken {
		taken[name] = true
	}

	for _, a := range schema.Attributes {
		taken[a.Name] = true
	}

	for _, b := range schema.Blocks {
		taken[b.Type] = true
	}

	remain := rest
	if sb, ok := rest.(*hclsyntax.Body); ok {
		remain = &freeForm{body: sb, taken: taken}
	}

	return wrapBlocks(content), remain, diags
}

// JustAttributes returns the attributes of the body and its blocks as
// attributes, without those the decoder took by a schema.
func (f *freeForm) JustAttributes() (hcl.Attributes, hcl.Diagnostics) {
	attrs := hcl.Attributes{}

	for name, a := range f.body.Attributes {
		if f.taken[name] {
			continue
		}

		attr := a.AsHCLAttribute()
		attr.Expr = attributeExpr(a.Expr)
		attrs[name] = attr
	}

	for _, group := range blockGroups(f.body.Blocks) {
		first := group[0]
		if f.taken[first.Type] {
			continue
		}

		attrs[first.Type] = &hcl.Attribute{
			Name:      first.Type,
			Expr:      &blocksExpr{blocks: group},
			Range:     hcl.RangeBetween(first.Range(), group[len(group)-1].Range()),
			NameRange: first.TypeRange,
		}
	}

	return attrs, nil
}

func (f *freeForm) MissingItemRange() hcl.Range { return f.body.MissingItemRange() }

// wrapBlocks reads the bodies of the blocks of content as free-form too.
func wrapBlocks(content *hcl.BodyContent) *hcl.BodyContent {
	if content == nil {
		return nil
	}

	for i, b := range content.Blocks {
		nb := *b
		nb.Body = asFreeForm(b.Body)
		content.Blocks[i] = &nb
	}

	return content
}

// blockGroups are the blocks of a body by type, in the order the types first
// appear, each type in the order of its blocks.
func blockGroups(blocks hclsyntax.Blocks) []hclsyntax.Blocks {
	var groups []hclsyntax.Blocks

	index := map[string]int{}

	for _, b := range blocks {
		i, ok := index[b.Type]
		if !ok {
			i = len(groups)
			index[b.Type] = i

			groups = append(groups, nil)
		}

		groups[i] = append(groups[i], b)
	}

	return groups
}

// attributeExpr is the expression of an attribute of a free-form body: an
// object constructor is a tuple of that one object.
func attributeExpr(expr hclsyntax.Expression) hcl.Expression {
	if _, ok := expr.(*hclsyntax.ObjectConsExpr); ok {
		return &tupleOfOne{expr: expr}
	}

	return expr
}

// tupleOfOne is an expression whose value is a tuple of the value of expr.
type tupleOfOne struct {
	expr hcl.Expression
}

func (e *tupleOfOne) Value(ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	v, diags := e.expr.Value(ctx)

	return cty.TupleVal([]cty.Value{v}), diags
}

func (e *tupleOfOne) Variables() []hcl.Traversal { return e.expr.Variables() }

func (e *tupleOfOne) Range() hcl.Range { return e.expr.Range() }

func (e *tupleOfOne) StartRange() hcl.Range { return e.expr.StartRange() }

// blocksExpr is the value of the blocks of one type of a free-form body.
type blocksExpr struct {
	blocks hclsyntax.Blocks
}

func (e *blocksExpr) Value(ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	values := make([]cty.Value, 0, len(e.blocks))

	var diags hcl.Diagnostics

	for _, b := range e.blocks {
		v, moreDiags := blockValue(b, ctx)
		diags = append(diags, moreDiags...)
		values = append(values, v)
	}

	return cty.TupleVal(values), diags
}

// blockValue is the value of one block: the object of its attributes and of
// its nested blocks, in objects keyed by its labels, the last label
// innermost.
func blockValue(b *hclsyntax.Block, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	attrs, _ := (&freeForm{body: b.Body}).JustAttributes()

	fields := make(map[string]cty.Value, len(attrs))

	var diags hcl.Diagnostics

	for name, a := range attrs {
		v, moreDiags := a.Expr.Value(ctx)
		diags = append(diags, moreDiags...)
		fields[name] = v
	}

	v := cty.ObjectVal(fields)

	for _, label := range slices.Backward(b.Labels) {
		v = cty.ObjectVal(map[string]cty.Value{label: cty.TupleVal([]cty.Value{v})})
	}

	return v, diags
}

func (e *blocksExpr) Variables() []hcl.Traversal {
	var out []hcl.Traversal

	for _, b := range e.blocks {
		out = append(out, bodyVariables(b.Body)...)
	}

	return out
}

// bodyVariables are the variables the attributes of a body and of its nested
// blocks refer to.
func bodyVariables(body *hclsyntax.Body) []hcl.Traversal {
	var out []hcl.Traversal

	for _, a := range body.Attributes {
		out = append(out, a.Expr.Variables()...)
	}

	for _, b := range body.Blocks {
		out = append(out, bodyVariables(b.Body)...)
	}

	return out
}

func (e *blocksExpr) Range() hcl.Range {
	return hcl.RangeBetween(e.blocks[0].Range(), e.blocks[len(e.blocks)-1].Range())
}

func (e *blocksExpr) StartRange() hcl.Range { return e.blocks[0].OpenBraceRange }
