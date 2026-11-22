// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// cty panics when the content of a marked value is read, and a value is
// marked here whenever it is sensitive. A place that reads the content goes
// through these helpers: a value that may be sensitive hands its marks to
// its parts, one that may not is refused before it is decoded.

// attributeValues returns the attributes of a known object or the elements
// of a known map, each with the marks of v: the parts of a value that is
// sensitive as a whole are sensitive.
func attributeValues(v cty.Value) map[string]cty.Value {
	inner, marks := v.Unmark()

	attrs := inner.AsValueMap()
	for name, item := range attrs {
		attrs[name] = item.WithMarks(marks)
	}

	return attrs
}

// refuseSensitive reports the attributes of body, of those named, that take
// a sensitive value; what says where that is. It runs before gohcl decodes
// them: gohcl panics on a marked value.
func refuseSensitive(body hcl.Body, ctx *hcl.EvalContext, what string, names ...string) hcl.Diagnostics {
	schema := &hcl.BodySchema{Attributes: make([]hcl.AttributeSchema, 0, len(names))}
	for _, name := range names {
		schema.Attributes = append(schema.Attributes, hcl.AttributeSchema{Name: name})
	}

	content, _, diags := body.PartialContent(schema)
	if diags.HasErrors() {
		return diags
	}

	for _, name := range names {
		attr, ok := content.Attributes[name]
		if !ok {
			continue
		}

		v, moreDiags := attr.Expr.Value(ctx)
		diags = append(diags, moreDiags...)

		if v.ContainsMarked() {
			diags = append(diags, sensitiveDiag(what, attr.Expr.Range()))
		}
	}

	return diags
}
