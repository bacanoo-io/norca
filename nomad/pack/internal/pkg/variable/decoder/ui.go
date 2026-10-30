// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/nomad/pack/internal/pkg/variable/schema"
	"github.com/hashicorp/nomad/pack/sdk/pack/variables"
	"github.com/zclconf/go-cty/cty"
)

// UIWidgets are the widgets of the install form (ui/app/utils/pack-form.js).
var UIWidgets = []string{
	"text", "number", "slider", "switch", "segmented", "select", "secret",
	"code", "tags", "key-value", "object-list", "namespace-select",
	"region-select", "node-pool-select", "datacenter-multiselect",
	"storage-volume-select",
}

// The ui attributes and the JSON Schema property keys they become.
var uiAttributeKeys = map[string]string{
	"tab":           "x-ui-tab",
	"group":         "x-ui-group",
	"order":         "x-ui-order",
	"label":         "title",
	"widget":        "x-ui-widget",
	"unit":          "x-ui-unit",
	"step":          "x-ui-step",
	"presets":       "x-ui-presets",
	"options":       "enum",
	"option_labels": "x-ui-enum-labels",
	"show_if":       "x-ui-show-if",
}

// The validation attributes and the JSON Schema keywords they become.
var validationAttributeKeys = map[string]string{
	"min":        "minimum",
	"max":        "maximum",
	"pattern":    "pattern",
	"min_length": "minLength",
	"max_length": "maxLength",
	"min_items":  "minItems",
	"max_items":  "maxItems",
	"unique":     "uniqueItems",
	// item_pattern and item_min_length go to "items".
}

// decodeFormBlocks decodes the ui, secret and validation blocks of a
// variable into its form directives:
//
//	variable "redis_password" {
//	  type = string
//	  ui {
//	    tab     = "security"
//	    group   = "Authentication"
//	    show_if = { field = "auth_enabled", equals = true }
//	  }
//
// A list of objects can name the choices of attributes of its items, which
// the install form shows as selects in the columns of its table:
//
//	ui {
//	  item_options = { protocol = ["http", "https", "tcp", "udp"] }
//	}
//	  secret {
//	    path = "nomad/jobs/redis"
//	    item = "password"
//	  }
//	  validation {
//	    min_length = 12
//	  }
//	}
func decodeFormBlocks(v *variables.Variable, blocks hcl.Blocks) hcl.Diagnostics {
	var diags hcl.Diagnostics
	seen := map[string]bool{}
	for _, b := range blocks {
		if seen[b.Type] {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("Duplicate %s block", b.Type),
				Detail:   fmt.Sprintf("A variable can have only one %s block.", b.Type),
				Subject:  b.DefRange.Ptr(),
			})
			continue
		}
		seen[b.Type] = true
		if v.UI == nil {
			v.UI = map[string]any{}
		}
		switch b.Type {
		case schema.VariableBlockUI:
			diags = diags.Extend(decodeUIBlock(v, b))
		case schema.VariableBlockSecret:
			diags = diags.Extend(decodeSecretBlock(v, b))
		case schema.VariableBlockValidation:
			diags = diags.Extend(decodeValidationBlock(v, b))
		}
	}
	return diags.Extend(checkSlider(v, blocks))
}

// checkSlider checks a variable with the slider widget: a number with a
// validation min below its max, the ends of the slider.
func checkSlider(v *variables.Variable, blocks hcl.Blocks) hcl.Diagnostics {
	if v.UI == nil || v.UI["x-ui-widget"] != "slider" {
		return nil
	}
	var subject *hcl.Range
	for _, b := range blocks {
		if b.Type == schema.VariableBlockUI {
			subject = b.DefRange.Ptr()
		}
	}
	problem := ""
	ty := v.ConstraintType
	minimum, hasMin := v.UI["minimum"]
	maximum, hasMax := v.UI["maximum"]
	switch {
	case ty != cty.NilType && ty != cty.DynamicPseudoType && ty != cty.Number:
		problem = "The slider widget is for variables of the type number."
	case !hasMin || !hasMax:
		problem = "The slider widget needs the min and max of a validation block: the ends of the slider."
	case toFloat(minimum) >= toFloat(maximum):
		problem = "The validation min of a slider must be below its max."
	}
	if problem == "" {
		return nil
	}
	return hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  "Invalid slider",
		Detail:   problem,
		Subject:  subject,
	}}
}

func toFloat(n any) float64 {
	switch n := n.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

func decodeUIBlock(v *variables.Variable, b *hcl.Block) hcl.Diagnostics {
	content, diags := b.Body.Content(schema.VariableUIBlockSchema)
	if content == nil {
		return diags
	}
	for name, attr := range content.Attributes {
		val, valDiags := attr.Expr.Value(nil)
		diags = diags.Extend(valDiags)
		if valDiags.HasErrors() {
			continue
		}
		out, err := uiAttributeValue(name, val)
		if err == "" && name == "item_options" {
			err = itemOptions(v, out)
		}
		if err != "" {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("Invalid ui %s", name),
				Detail:   err,
				Subject:  attr.Expr.Range().Ptr(),
			})
			continue
		}
		if name == "item_options" {
			continue
		}
		v.UI[uiAttributeKeys[name]] = out
	}
	return diags
}

// uiAttributeValue checks the value of a ui attribute and returns it as
// JSON Schema, or an error text.
func uiAttributeValue(name string, val cty.Value) (any, string) {
	if val.IsNull() {
		return nil, "The value must not be null."
	}
	ty := val.Type()
	switch name {
	case "tab", "group", "label", "unit":
		if ty != cty.String {
			return nil, "The value must be a string."
		}
		if name == "tab" {
			return strings.ToLower(strings.TrimSpace(val.AsString())), ""
		}
		return val.AsString(), ""
	case "widget":
		if ty != cty.String || !slices.Contains(UIWidgets, val.AsString()) {
			return nil, fmt.Sprintf("The widget must be one of %s.", strings.Join(UIWidgets, ", "))
		}
		return val.AsString(), ""
	case "order":
		if ty != cty.Number {
			return nil, "The value must be a number."
		}
	case "step":
		if ty != cty.Number || val.AsBigFloat().Sign() <= 0 {
			return nil, "The step must be a number above 0."
		}
	case "presets", "options":
		if !ty.IsListType() && !ty.IsTupleType() && !ty.IsSetType() {
			return nil, "The value must be a list."
		}
		if val.LengthInt() == 0 {
			return nil, "The list must not be empty."
		}
	case "option_labels":
		if !ty.IsObjectType() && !ty.IsMapType() {
			return nil, `The value must be a map of option to label, such as { replica = "Primary + Replicas" }.`
		}
	case "item_options":
		if !ty.IsObjectType() && !ty.IsMapType() {
			return nil, `The value must be a map of attribute to choices, such as { protocol = ["http", "tcp"] }.`
		}
	case "show_if":
		// A variable name, shown when the variable is set (truthy), or
		// { field = "...", equals | notEquals | in = ... }.
		if ty == cty.String {
			return map[string]any{"field": val.AsString()}, ""
		}
		if !ty.IsObjectType() || !ty.HasAttribute("field") {
			return nil, `The value must be a variable name or an object such as { field = "mode", equals = "replica" }; tests are equals, notEquals and in.`
		}
		tests := 0
		for _, k := range []string{"equals", "notEquals", "in"} {
			if ty.HasAttribute(k) {
				tests++
			}
		}
		if tests > 1 || len(ty.AttributeTypes()) != tests+1 {
			return nil, "The object has a field and at most one of equals, notEquals and in."
		}
	}
	out, err := variables.ConvertCtyToInterface(val)
	if err != nil {
		return nil, err.Error()
	}
	return out, ""
}

// itemOptions adds the choices of attributes of the items of a list of
// objects to the item schema of the variable (items.properties.<attr>.enum),
// where the form and the validation of the values find them. It returns an
// error text.
func itemOptions(v *variables.Variable, value any) string {
	options, _ := value.(map[string]any)
	ty := v.ConstraintType
	if ty == cty.NilType {
		ty = v.Type
	}
	var attrs map[string]cty.Type
	if ty.IsListType() || ty.IsSetType() {
		if elem := ty.ElementType(); elem.IsObjectType() {
			attrs = elem.AttributeTypes()
		}
	}
	if attrs == nil {
		return "item_options is for variables of a list of objects."
	}
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	slices.Sort(names)
	props := map[string]any{}
	for _, name := range names {
		at, ok := attrs[name]
		if !ok {
			return fmt.Sprintf("The items have no attribute %q.", name)
		}
		if !at.IsPrimitiveType() {
			return fmt.Sprintf("The attribute %q is not a string, number or bool.", name)
		}
		choices, ok := options[name].([]any)
		if !ok || len(choices) == 0 {
			return fmt.Sprintf("The choices of %q must be a non-empty list.", name)
		}
		props[name] = map[string]any{"enum": choices}
	}
	items, _ := v.UI["items"].(map[string]any)
	if items == nil {
		items = map[string]any{}
		v.UI["items"] = items
	}
	items["properties"] = props
	return ""
}

func decodeValidationBlock(v *variables.Variable, b *hcl.Block) hcl.Diagnostics {
	content, diags := b.Body.Content(schema.VariableValidationBlockSchema)
	if content == nil {
		return diags
	}
	for name, attr := range content.Attributes {
		val, valDiags := attr.Expr.Value(nil)
		diags = diags.Extend(valDiags)
		if valDiags.HasErrors() {
			continue
		}
		var problem string
		switch {
		case val.IsNull():
			problem = "The value must not be null."
		case name == "integer" || name == "unique":
			if val.Type() != cty.Bool {
				problem = "The value must be true or false."
			}
		case name == "pattern" || name == "item_pattern":
			if val.Type() != cty.String {
				problem = "The value must be a regular expression string."
			} else if _, err := regexp.Compile(val.AsString()); err != nil {
				problem = fmt.Sprintf("The pattern is not a valid regular expression: %v.", err)
			}
		case val.Type() != cty.Number:
			problem = "The value must be a number."
		case name != "min" && name != "max":
			if f, _ := val.AsBigFloat().Float64(); f < 0 || f != float64(int64(f)) {
				problem = "The value must be a whole number, 0 or more."
			}
		}
		if problem != "" {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("Invalid validation %s", name),
				Detail:   problem,
				Subject:  attr.Expr.Range().Ptr(),
			})
			continue
		}
		switch name {
		case "integer":
			if val.True() {
				v.UI["type"] = "integer"
			}
			continue
		case "unique":
			if val.True() {
				v.UI["uniqueItems"] = true
			}
			continue
		case "item_pattern", "item_min_length":
			items, _ := v.UI["items"].(map[string]any)
			if items == nil {
				items = map[string]any{}
				v.UI["items"] = items
			}
			key := "pattern"
			if name == "item_min_length" {
				key = "minLength"
			}
			out, _ := variables.ConvertCtyToInterface(val)
			items[key] = out
			continue
		}
		out, err := variables.ConvertCtyToInterface(val)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Invalid validation " + name, Detail: err.Error(), Subject: attr.Expr.Range().Ptr()})
			continue
		}
		v.UI[validationAttributeKeys[name]] = out
	}
	return diags
}

func decodeSecretBlock(v *variables.Variable, b *hcl.Block) hcl.Diagnostics {
	content, diags := b.Body.Content(schema.VariableSecretBlockSchema)
	if content == nil {
		return diags
	}
	str := func(name string) string {
		attr, ok := content.Attributes[name]
		if !ok {
			return ""
		}
		val, valDiags := attr.Expr.Value(nil)
		diags = diags.Extend(valDiags)
		if valDiags.HasErrors() {
			return ""
		}
		if val.IsNull() || val.Type() != cty.String || strings.TrimSpace(val.AsString()) == "" {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("Invalid secret %s", name),
				Detail:   "The value must be a non-empty string.",
				Subject:  attr.Expr.Range().Ptr(),
			})
			return ""
		}
		return strings.TrimSpace(val.AsString())
	}
	path, item := str("path"), str("item")
	if path == "" {
		return diags
	}
	if item == "" {
		item = v.Name.String()
	}
	v.UI["x-ui-secret-path"] = path
	v.UI["x-ui-secret-item"] = item
	return diags
}
