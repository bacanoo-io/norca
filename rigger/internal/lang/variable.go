// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"fmt"
	"regexp"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// Variable is a variable block of variables.hcl.
type Variable struct {
	Name        string
	Description string
	// Type is the type constraint; cty.DynamicPseudoType for any.
	Type cty.Type
	// TypeText is the type constraint as written.
	TypeText string
	// Defaults are the defaults of optional attributes of the type.
	Defaults *typeexpr.Defaults
	// Default is the default value, cty.NilVal for none.
	Default   cty.Value
	Sensitive bool
	// Validation is the validation block, nil for none.
	Validation *Constraint
	// UI holds the attributes of the ui block, for the install form.
	UI map[string]cty.Value
	// Secret says where the secret store holds the value, nil for none.
	Secret *Secret
	// DeclRange is the range of the block header.
	DeclRange hcl.Range
}

// Secret is the secret block of a variable (design 6.3).
type Secret struct {
	Path string
	Item string
}

// Rule is a rule block: a condition over the variables (design 6.1).
type Rule struct {
	Name         string
	Condition    hcl.Expression
	ErrorMessage string
	DeclRange    hcl.Range
}

// variableName is the name of a variable, a rule or a local.
var variableName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

var variablesSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "variable", LabelNames: []string{labelName}},
		{Type: "rule", LabelNames: []string{labelName}},
	},
}

var variableSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "type"}, {Name: "default"}, {Name: "description"}, {Name: "sensitive"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "validation"}, {Type: "ui"}, {Type: "secret"},
	},
}

var ruleSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "condition", Required: true},
		{Name: "error_message", Required: true},
	},
}

// uiSchema holds the hints of the install form a variable may give
// (design 2.1); an unknown widget falls back to a text field in the form.
var uiSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "tab"},
		{Name: "group"},
		{Name: "order"},
		{Name: "label"},
		{Name: "widget"},
		{Name: "unit"},
		{Name: "presets"},
		{Name: "option_labels"},
		{Name: "show_if"},
		{Name: "help"},
		{Name: "placeholder"},
	},
}

var secretSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "path", Required: true},
		{Name: "item", Required: true},
	},
}

// decodeVariables decodes variables.hcl into m.
func (m *Module) decodeVariables(body hcl.Body) hcl.Diagnostics {
	content, diags := body.Content(variablesSchema)
	seen := map[string]hcl.Range{}

	for _, block := range content.Blocks {
		name := block.Labels[0]
		if !variableName.MatchString(name) {
			diags = append(diags, errorAt(block.LabelRanges[0], "Invalid name",
				fmt.Sprintf("%q is not a name: lower case letters, digits and _, starting with a letter or _.", name)))

			continue
		}

		key := block.Type + "." + name
		if prev, dup := seen[key]; dup {
			diags = append(diags, errorAt(block.DefRange, "Duplicate "+block.Type,
				fmt.Sprintf("%s %q is declared already at %s.", block.Type, name, prev)))

			continue
		}

		seen[key] = block.DefRange

		if block.Type == "rule" {
			r, moreDiags := decodeRule(block)
			diags = append(diags, moreDiags...)
			m.Rules = append(m.Rules, r)

			continue
		}

		v, moreDiags := decodeVariable(block)
		diags = append(diags, moreDiags...)
		m.Variables = append(m.Variables, v)
	}

	return diags
}

func decodeRule(block *hcl.Block) (*Rule, hcl.Diagnostics) {
	content, diags := block.Body.Content(ruleSchema)
	r := &Rule{Name: block.Labels[0], DeclRange: block.DefRange}

	if attr, ok := content.Attributes["condition"]; ok {
		r.Condition = attr.Expr
	}

	if attr, ok := content.Attributes["error_message"]; ok {
		v, moreDiags := attr.Expr.Value(nil)
		diags = append(diags, moreDiags...)

		if !moreDiags.HasErrors() {
			s, moreDiags := asString(attr, v)
			diags = append(diags, moreDiags...)
			r.ErrorMessage = s
		}
	}

	return r, diags
}

func decodeVariable(block *hcl.Block) (*Variable, hcl.Diagnostics) {
	content, diags := block.Body.Content(variableSchema)
	v := &Variable{
		Name:      block.Labels[0],
		Type:      cty.DynamicPseudoType,
		TypeText:  "any",
		Default:   cty.NilVal,
		DeclRange: block.DefRange,
	}

	diags = append(diags, v.decodeAttributes(content.Attributes)...)

	for _, b := range content.Blocks {
		diags = append(diags, v.decodeBlock(b)...)
	}

	if v.Validation != nil {
		diags = append(diags, v.Validation.fits(v.Type)...)
	}

	return v, diags
}

func (v *Variable) decodeAttributes(attrs hcl.Attributes) hcl.Diagnostics {
	var diags hcl.Diagnostics

	if attr, ok := attrs["type"]; ok {
		ty, defaults, moreDiags := typeexpr.TypeConstraintWithDefaults(attr.Expr)
		diags = append(diags, moreDiags...)
		v.Type, v.Defaults = ty, defaults
		v.TypeText = typeexpr.TypeString(ty)
	}

	for _, name := range []string{"description", "sensitive"} {
		attr, ok := attrs[name]
		if !ok {
			continue
		}

		val, moreDiags := attr.Expr.Value(nil)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		if name == "description" {
			v.Description, moreDiags = asString(attr, val)
		} else {
			v.Sensitive, moreDiags = asBool(attr, val)
		}

		diags = append(diags, moreDiags...)
	}

	if attr, ok := attrs["default"]; ok {
		diags = append(diags, v.decodeDefault(attr)...)
	}

	return diags
}

// decodeDefault decodes the default, which must have the type of the
// variable. The defaults of optional attributes apply to it as to a given
// value.
func (v *Variable) decodeDefault(attr *hcl.Attribute) hcl.Diagnostics {
	val, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		return diags
	}

	if v.Defaults != nil {
		val = v.Defaults.Apply(val)
	}

	converted, err := convert.Convert(val, v.Type)
	if err != nil {
		return append(diags, errorAt(attr.Expr.Range(), "Invalid default",
			fmt.Sprintf("The default is not a %s: %s.", v.Type.FriendlyName(), err)))
	}

	v.Default = converted

	return diags
}

func (v *Variable) decodeBlock(b *hcl.Block) hcl.Diagnostics {
	switch b.Type {
	case "validation":
		c, diags := decodeConstraint(b)
		v.Validation = c

		return diags
	case "ui":
		ui, diags := decodeConstAttributes(b.Body, uiSchema)
		v.UI = ui

		return diags
	default:
		attrs, diags := decodeConstAttributes(b.Body, secretSchema)
		v.Secret = &Secret{}

		for name, val := range attrs {
			s, ok := stringOf(val)
			if !ok {
				diags = append(diags, errorAt(b.DefRange, "Invalid secret", "secret."+name+" must be a string."))

				continue
			}

			if name == "path" {
				v.Secret.Path = s
			} else {
				v.Secret.Item = s
			}
		}

		return diags
	}
}

// decodeConstAttributes decodes a block of constant attributes.
func decodeConstAttributes(body hcl.Body, schema *hcl.BodySchema) (map[string]cty.Value, hcl.Diagnostics) {
	content, diags := body.Content(schema)
	out := make(map[string]cty.Value, len(content.Attributes))

	for name, attr := range content.Attributes {
		val, moreDiags := attr.Expr.Value(nil)
		diags = append(diags, moreDiags...)

		if !moreDiags.HasErrors() {
			out[name] = val
		}
	}

	return out, diags
}

// stringOf returns the string a known, non-null string value holds.
func stringOf(v cty.Value) (string, bool) {
	if v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return "", false
	}

	return v.AsString(), true
}
