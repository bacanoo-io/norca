// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// walker evaluates bodies of the native syntax with no schema, and notes
// where a sensitive value lands.
type walker struct {
	// skip are the names left out of the top body: the meta-arguments of a
	// resource.
	skip map[string]bool
	// marked are the attributes whose value holds a sensitive value.
	marked []markedAttr
	// test gives the value a test reads (design 7.4): an attribute that is
	// null is kept, and a dynamic block that makes no block is an empty
	// list (an empty object with labels). A resource leaves both out.
	test bool
}

// markedAttr is an attribute whose value holds a sensitive value: its path
// from the top body (block types and labels, then the attribute name).
type markedAttr struct {
	path []string
	rng  hcl.Range
}

// value returns the value of a body: its attributes as they evaluate (a null
// attribute is left out, as if it were not written), and its blocks,
// expanded from dynamic blocks, by type. Blocks with a label are an object by
// label (by the first label, then the second); blocks without one are an
// object when the type is written once, and a tuple when it is written more
// than once or comes from a dynamic block.
func (w *walker) value(body hcl.Body, ctx *hcl.EvalContext) (cty.Value, hcl.Diagnostics) {
	return w.body(body, ctx, nil)
}

func (w *walker) body(body hcl.Body, ctx *hcl.EvalContext, path []string) (cty.Value, hcl.Diagnostics) {
	// The walk leaves null attributes out by itself.
	if nn, ok := body.(*withoutNulls); ok {
		body = nn.body
	}

	syntax, ok := body.(*hclsyntax.Body)
	if !ok {
		return cty.DynamicVal, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Unsupported syntax",
			Detail: "A resource is written in the native syntax of HCL.",
		}}
	}

	values := map[string]cty.Value{}

	var diags hcl.Diagnostics

	for name, attr := range syntax.Attributes {
		if path == nil && w.skip[name] {
			continue
		}

		v, moreDiags := attr.Expr.Value(ctx)
		diags = append(diags, moreDiags...)

		if v.ContainsMarked() {
			w.marked = append(w.marked, markedAttr{path: append(slices.Clone(path), name), rng: attr.Expr.Range()})
		}

		if !v.IsNull() || w.test {
			values[name] = v
		}
	}

	blocks, moreDiags := w.blocks(syntax.Blocks, ctx, path)
	diags = append(diags, moreDiags...)

	if w.test {
		w.emptyDynamics(syntax.Blocks, path, values)
	}

	diags = append(diags, addBlocks(values, blocks)...)

	return cty.ObjectVal(values), diags
}

// emptyDynamics gives the type of a dynamic block that may make no block
// its empty value, as a test reads it: an empty list, or an empty object
// when the blocks have labels. A block it makes replaces it.
func (w *walker) emptyDynamics(blocks hclsyntax.Blocks, path []string, values map[string]cty.Value) {
	for _, b := range blocks {
		if b.Type != "dynamic" || len(b.Labels) != 1 || (path == nil && w.skip[b.Labels[0]]) {
			continue
		}

		if _, done := values[b.Labels[0]]; done {
			continue
		}

		if _, labelled := b.Body.Attributes["labels"]; labelled {
			values[b.Labels[0]] = cty.EmptyObjectVal
		} else {
			values[b.Labels[0]] = cty.EmptyTupleVal
		}
	}
}

// addBlocks adds the evaluated blocks to the values of a body, by type.
func addBlocks(values map[string]cty.Value, blocks []evaluatedBlock) hcl.Diagnostics {
	var diags hcl.Diagnostics

	types := []string{}
	byType := map[string][]evaluatedBlock{}

	for _, b := range blocks {
		if _, seen := byType[b.typ]; !seen {
			types = append(types, b.typ)
		}

		byType[b.typ] = append(byType[b.typ], b)
	}

	for _, typ := range types {
		if v, clash := values[typ]; clash && !isEmptyCollection(v) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Attribute and block of one name",
				Detail:  typ + " is an attribute and a block of the same body.",
				Subject: byType[typ][0].rng.Ptr(),
			})

			continue
		}

		values[typ] = blocksValue(byType[typ])
	}

	return diags
}

// evaluatedBlock is a block with its labels and its value.
type evaluatedBlock struct {
	typ       string
	labels    []string
	value     cty.Value
	isDynamic bool
	rng       hcl.Range
}

// expandBlocks evaluates the blocks of a body; a dynamic block gives one
// block per item of its for_each.
func (w *walker) blocks(blocks hclsyntax.Blocks, ctx *hcl.EvalContext, path []string) ([]evaluatedBlock, hcl.Diagnostics) {
	var (
		out   []evaluatedBlock
		diags hcl.Diagnostics
	)

	for _, b := range blocks {
		if path == nil && w.skip[b.Type] {
			continue
		}

		if b.Type == "dynamic" {
			more, moreDiags := w.dynamic(b, ctx, path)
			out = append(out, more...)
			diags = append(diags, moreDiags...)

			continue
		}

		v, moreDiags := w.body(b.Body, ctx, blockPath(path, b.Type, b.Labels))
		diags = append(diags, moreDiags...)
		out = append(out, evaluatedBlock{typ: b.Type, labels: b.Labels, value: v, rng: b.DefRange()})
	}

	return out, diags
}

// expandDynamic expands a dynamic block: for_each, iterator, labels and a
// content block, as HCL's dynamic blocks are written.
func (w *walker) dynamic(b *hclsyntax.Block, ctx *hcl.EvalContext, path []string) ([]evaluatedBlock, hcl.Diagnostics) {
	if len(b.Labels) != 1 {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Invalid dynamic block",
			Detail: "A dynamic block has one label: the type of the blocks it makes.", Subject: b.DefRange().Ptr(),
		}}
	}

	typ := b.Labels[0]
	iterator := typ

	if attr, ok := b.Body.Attributes["iterator"]; ok {
		iterator = hcl.ExprAsKeyword(attr.Expr)
	}

	attr, ok := b.Body.Attributes["for_each"]
	if !ok {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Invalid dynamic block",
			Detail: "A dynamic block has a for_each.", Subject: b.DefRange().Ptr(),
		}}
	}

	forEach, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, diags
	}

	if forEach.ContainsMarked() {
		return nil, append(diags, sensitiveDiag("the for_each of a dynamic block", attr.Expr.Range()))
	}

	content := slices.IndexFunc(b.Body.Blocks, func(c *hclsyntax.Block) bool { return c.Type == "content" })
	if content < 0 || forEach.IsNull() || !forEach.CanIterateElements() {
		return nil, append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Invalid dynamic block",
			Detail: "A dynamic block has a for_each that is a collection and a content block.", Subject: b.DefRange().Ptr(),
		})
	}

	var out []evaluatedBlock

	for it := forEach.ElementIterator(); it.Next(); {
		key, value := it.Element()
		child := ctx.NewChild()
		child.Variables = map[string]cty.Value{
			iterator: cty.ObjectVal(map[string]cty.Value{attrKey: key, attrValue: value}),
		}

		labels, moreDiags := dynamicLabels(b, child)
		diags = append(diags, moreDiags...)

		v, moreDiags := w.body(b.Body.Blocks[content].Body, child, blockPath(path, typ, labels))
		diags = append(diags, moreDiags...)
		out = append(out, evaluatedBlock{typ: typ, labels: labels, value: v, isDynamic: true, rng: b.DefRange()})
	}

	return out, diags
}

// dynamicLabels evaluates the labels of a dynamic block.
func dynamicLabels(b *hclsyntax.Block, ctx *hcl.EvalContext) ([]string, hcl.Diagnostics) {
	attr, ok := b.Body.Attributes["labels"]
	if !ok {
		return nil, nil
	}

	v, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() || v.IsNull() || !v.CanIterateElements() {
		return nil, diags
	}

	var labels []string

	for it := v.ElementIterator(); it.Next(); {
		_, l := it.Element()
		if s, ok := stringValue(l); ok {
			labels = append(labels, s)
		}
	}

	return labels, diags
}

// blocksValue returns the value of the blocks of one type.
func blocksValue(blocks []evaluatedBlock) cty.Value {
	if len(blocks[0].labels) > 0 {
		return labelledValue(blocks, 0)
	}

	if len(blocks) == 1 && !blocks[0].isDynamic {
		return blocks[0].value
	}

	values := make([]cty.Value, len(blocks))
	for i, b := range blocks {
		values[i] = b.value
	}

	return cty.TupleVal(values)
}

// labelledValue returns an object of the blocks by their label at depth.
func labelledValue(blocks []evaluatedBlock, depth int) cty.Value {
	byLabel := map[string][]evaluatedBlock{}

	for _, b := range blocks {
		if depth < len(b.labels) {
			byLabel[b.labels[depth]] = append(byLabel[b.labels[depth]], b)
		}
	}

	values := map[string]cty.Value{}

	for label, group := range byLabel {
		if depth+1 < len(group[0].labels) {
			values[label] = labelledValue(group, depth+1)

			continue
		}

		values[label] = group[len(group)-1].value
	}

	return cty.ObjectVal(values)
}

// isEmptyCollection reports whether v is the empty value of a dynamic block
// that made no block.
func isEmptyCollection(v cty.Value) bool {
	return v.RawEquals(cty.EmptyTupleVal) || v.RawEquals(cty.EmptyObjectVal)
}

// blockPath is the path of a block under path.
func blockPath(path []string, typ string, labels []string) []string {
	return append(append(slices.Clone(path), typ), labels...)
}

func stringValue(v cty.Value) (string, bool) {
	v, _ = v.Unmark()
	if v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return "", false
	}

	return v.AsString(), true
}
