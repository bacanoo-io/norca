// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"encoding/json"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/internal/syntax"
)

// keyType is the key of the type of a JSON Schema, jsonObject the type of
// an object.
const (
	keyType    = "type"
	jsonObject = "object"
)

// FormSchema returns the install form of the module: a JSON Schema of its
// variables with the x-ui-* hints the web UI of Norca renders (its
// schema-form, which the install form of packs uses too;
// design/rig-integration.md 4.2 of norca). It is built from the types, the
// validation and the ui block of each variable and the tabs of the
// metadata:
//
//   - a property per variable, with its type (list and set as arrays, map
//     as an object of additional properties, object with its attributes),
//     description, default (not of a sensitive variable), and the
//     constraints of its validation (pattern, lengths, range, integer,
//     items, unique, one_of as enum, item_of, key_pattern, attributes);
//   - required: the variables without a default;
//   - x-ui-tabs: the tabs of the metadata; per property x-ui-tab, x-ui-group,
//     x-ui-order (the order of variables.hcl unless the ui block gives
//     one), title (label), x-ui-widget, x-ui-unit, x-ui-presets,
//     x-ui-enum-labels (option_labels), x-ui-placeholder, x-ui-help;
//   - a variable with a secret block is a secret field (x-ui-widget secret,
//     x-ui-secret-path, x-ui-secret-item); a sensitive one is x-ui-secret;
//   - show_if: the simple conditions the form evaluates are translated
//     ([op, field, value]: a variable, its negation, == or != a literal);
//     any other condition is kept as written in x-ui-show-if-hcl, and the
//     form then shows the field always.
func (m *Module) FormSchema() map[string]any {
	props := make(map[string]any, len(m.mod.Variables))
	required := []string{}

	for i, v := range m.mod.Variables {
		props[v.Name] = variableSchema(v, (i+1)*10)

		if v.Default == cty.NilVal {
			required = append(required, v.Name)
		}
	}

	out := map[string]any{keyType: jsonObject, "properties": props, "required": required}

	if len(m.mod.Tabs) > 0 {
		tabs := make([]map[string]any, 0, len(m.mod.Tabs))
		for i, t := range m.mod.Tabs {
			tab := map[string]any{"id": t.Name, "label": t.Label, "order": i}
			if t.Icon != "" {
				tab["icon"] = t.Icon
			}

			tabs = append(tabs, tab)
		}

		out["x-ui-tabs"] = tabs
	}

	return out
}

// variableSchema is the property of one variable; order is its place in
// variables.hcl.
func variableSchema(v *lang.Variable, order int) map[string]any {
	p := typeSchema(v.Type)

	if v.Description != "" {
		p["description"] = v.Description
	}

	if v.Default != cty.NilVal && !v.Sensitive {
		if d, ok := plain(v.Default); ok {
			p["default"] = d
		}
	}

	constrain(p, v.Validation)

	p["x-ui-order"] = order
	uiHints(p, v.UI)

	if v.Secret != nil {
		p["x-ui-widget"] = "secret"
		p["x-ui-secret-path"] = v.Secret.Path
		p["x-ui-secret-item"] = v.Secret.Item
	}

	if v.Sensitive {
		p["x-ui-secret"] = true
	}

	return p
}

// typeSchema is the JSON Schema of a type constraint.
func typeSchema(t cty.Type) map[string]any {
	switch {
	case t == cty.String:
		return map[string]any{keyType: "string"}
	case t == cty.Number:
		return map[string]any{keyType: "number"}
	case t == cty.Bool:
		return map[string]any{keyType: "boolean"}
	case t.IsListType():
		return map[string]any{keyType: "array", "items": typeSchema(t.ElementType())}
	case t.IsSetType():
		return map[string]any{keyType: "array", "items": typeSchema(t.ElementType()), "uniqueItems": true}
	case t.IsTupleType():
		return map[string]any{keyType: "array"}
	case t.IsMapType():
		return map[string]any{keyType: jsonObject, "additionalProperties": typeSchema(t.ElementType())}
	case t.IsObjectType():
		props := map[string]any{}
		required := []string{}

		for name, at := range t.AttributeTypes() {
			props[name] = typeSchema(at)
			if !t.AttributeOptional(name) {
				required = append(required, name)
			}
		}

		slices.Sort(required)

		return map[string]any{keyType: jsonObject, "properties": props, "required": required}
	default:
		return map[string]any{}
	}
}

// constrain adds the constraints of a validation block to a schema.
func constrain(p map[string]any, c *lang.Constraint) {
	if c == nil {
		return
	}

	constrainValue(p, c)
	constrainCollection(p, c)
	constrainAttributes(p, c.Attributes)
}

// constrainValue adds the constraints of a single value: pattern, lengths,
// range, integer, one_of.
func constrainValue(p map[string]any, c *lang.Constraint) {
	if c.Pattern != nil {
		p["pattern"] = c.Pattern.String()
	}

	setInt(p, "minLength", c.MinLength)
	setInt(p, "maxLength", c.MaxLength)

	if c.Min != nil {
		f, _ := c.Min.Float64()
		p["minimum"] = f
	}

	if c.Max != nil {
		f, _ := c.Max.Float64()
		p["maximum"] = f
	}

	if c.Integer && p[keyType] == "number" {
		p[keyType] = "integer"
	}

	if enum := plainList(c.OneOf); enum != nil {
		p["enum"] = enum
	}
}

// constrainCollection adds the constraints of a list, a set or a map: the
// number of items, unique, the constraints of every item, the keys.
func constrainCollection(p map[string]any, c *lang.Constraint) {
	if p[keyType] == jsonObject {
		setInt(p, "minProperties", c.MinItems)
		setInt(p, "maxProperties", c.MaxItems)
	} else {
		setInt(p, "minItems", c.MinItems)
		setInt(p, "maxItems", c.MaxItems)
	}

	if c.Unique {
		p["uniqueItems"] = true
	}

	if c.UniqueBy != "" {
		p["x-ui-unique-by"] = c.UniqueBy
	}

	if items, ok := p["items"].(map[string]any); ok {
		if enum := plainList(c.ItemOf); enum != nil {
			items["enum"] = enum
		}

		constrain(items, c.Item)
		constrainAttributes(items, c.Attributes)
	} else if extra, ok := p["additionalProperties"].(map[string]any); ok {
		constrain(extra, c.Item)
	}

	if c.KeyPattern != nil {
		p["propertyNames"] = map[string]any{"pattern": c.KeyPattern.String()}
	}
}

// constrainAttributes adds the constraints of attribute blocks to the
// properties of an object schema.
func constrainAttributes(p map[string]any, attrs map[string]*lang.Constraint) {
	props, ok := p["properties"].(map[string]any)
	if !ok {
		return
	}

	for name, c := range attrs {
		if prop, ok := props[name].(map[string]any); ok {
			constrain(prop, c)
		}
	}
}

func setInt(p map[string]any, key string, n *int) {
	if n != nil {
		p[key] = *n
	}
}

// uiHints adds the hints of a ui block.
func uiHints(p map[string]any, ui map[string]cty.Value) {
	keys := map[string]string{
		"tab": "x-ui-tab", "group": "x-ui-group", "order": "x-ui-order", "label": "title",
		"widget": "x-ui-widget", "unit": "x-ui-unit", "presets": "x-ui-presets",
		"option_labels": "x-ui-enum-labels", "help": "x-ui-help", "placeholder": "x-ui-placeholder",
	}

	for name, val := range ui {
		if name == "show_if" {
			showIf(p, val)

			continue
		}

		if key, ok := keys[name]; ok {
			if v, ok := plain(val); ok {
				p[key] = v
			}
		}
	}
}

// showIf translates the condition of a ui block into the predicate the
// form evaluates, [op, field, value]; a condition the form can't evaluate
// is kept as written.
func showIf(p map[string]any, val cty.Value) {
	if !val.IsKnown() || val.IsNull() || val.Type() != cty.String {
		return
	}

	text := val.AsString()

	expr, diags := syntax.ParseExpression([]byte(text), "show_if")
	if !diags.HasErrors() {
		if pred, ok := predicate(expr); ok {
			p["x-ui-show-if"] = pred

			return
		}
	}

	p["x-ui-show-if-hcl"] = text
}

// predicate is the form's predicate of an expression, if it has one.
func predicate(expr hclsyntax.Expression) ([]any, bool) {
	switch e := expr.(type) {
	case *hclsyntax.ScopeTraversalExpr:
		if name, ok := variableName(e); ok {
			return []any{"truthy", name}, true
		}
	case *hclsyntax.UnaryOpExpr:
		if e.Op == hclsyntax.OpLogicalNot {
			if t, ok := e.Val.(*hclsyntax.ScopeTraversalExpr); ok {
				if name, ok := variableName(t); ok {
					return []any{"falsy", name}, true
				}
			}
		}
	case *hclsyntax.BinaryOpExpr:
		return comparison(e)
	case *hclsyntax.TemplateWrapExpr:
		return predicate(e.Wrapped)
	}

	return nil, false
}

// comparison is the predicate of a variable compared with a constant
// (== or !=).
func comparison(e *hclsyntax.BinaryOpExpr) ([]any, bool) {
	op := map[*hclsyntax.Operation]string{hclsyntax.OpEqual: "==", hclsyntax.OpNotEqual: "!="}[e.Op]
	if op == "" {
		return nil, false
	}

	t, ok := e.LHS.(*hclsyntax.ScopeTraversalExpr)
	if !ok || len(e.RHS.Variables()) > 0 {
		return nil, false
	}

	// The right side is a constant: a quoted string is a template.
	lit, diags := e.RHS.Value(nil)
	if diags.HasErrors() {
		return nil, false
	}

	name, ok := variableName(t)
	value, isPlain := plain(lit)

	if !ok || !isPlain {
		return nil, false
	}

	return []any{op, name, value}, true
}

// variableName is the variable a traversal names: name or var.name.
func variableName(t *hclsyntax.ScopeTraversalExpr) (string, bool) {
	switch len(t.Traversal) {
	case 1:
		return t.Traversal.RootName(), true
	case 2:
		if attr, ok := t.Traversal[1].(hcl.TraverseAttr); ok && t.Traversal.RootName() == "var" {
			return attr.Name, true
		}
	}

	return "", false
}

// plain returns a known value as plain JSON data.
func plain(v cty.Value) (any, bool) {
	if !v.IsWhollyKnown() {
		return nil, false
	}

	data, err := ctyjson.Marshal(v, v.Type())
	if err != nil {
		return nil, false
	}

	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false
	}

	return out, true
}

// plainList returns values as plain JSON data, nil for none.
func plainList(vals []cty.Value) []any {
	if len(vals) == 0 {
		return nil
	}

	out := make([]any, 0, len(vals))

	for _, v := range vals {
		if p, ok := plain(v); ok {
			out = append(out, p)
		}
	}

	return out
}
