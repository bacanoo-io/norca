// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"errors"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
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

// errHidden is the error of a function that failed with a sensitive
// argument: the error of the function may quote it.
var errHidden = errors.New("it failed on a sensitive value, so the reason is not shown")

// hideSensitive returns f with the error of a call hidden when an argument
// is sensitive. The errors of the standard functions quote their arguments,
// and a diagnostic goes to the response and the log of a server. The
// parameters let everything through: f keeps its own rules for null,
// unknown and marked arguments.
func hideSensitive(f function.Function) function.Function {
	passAll := func(p function.Parameter) function.Parameter {
		p.AllowNull, p.AllowUnknown, p.AllowDynamicType, p.AllowMarked = true, true, true, true

		return p
	}

	spec := &function.Spec{
		Description: f.Description(),
		Params:      f.Params(),
		Type: func(args []cty.Value) (cty.Type, error) {
			ty, err := f.ReturnTypeForValues(args)

			return ty, hiddenError(args, err)
		},
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			v, err := f.Call(args)

			return v, hiddenError(args, err)
		},
	}

	for i, p := range spec.Params {
		spec.Params[i] = passAll(p)
	}

	if vp := f.VarParam(); vp != nil {
		p := passAll(*vp)
		spec.VarParam = &p
	}

	return function.New(spec)
}

// hiddenError is err, or without its text when an argument is sensitive.
func hiddenError(args []cty.Value, err error) error {
	if err == nil || !slices.ContainsFunc(args, cty.Value.ContainsMarked) {
		return err
	}

	if argErr, ok := errors.AsType[function.ArgError](err); ok {
		return function.NewArgError(argErr.Index, errHidden)
	}

	return errHidden
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
