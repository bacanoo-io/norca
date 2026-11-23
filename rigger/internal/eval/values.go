// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package eval evaluates a module of Rigger: the values of its variables and
// its rules now; its locals, resources and outputs as phase 1 goes on
// (design 4.2, 6.1).
package eval

import (
	"bytes"
	"fmt"
	"slices"
	"sort"

	"github.com/agext/levenshtein"
	"github.com/hashicorp/hcl/v2"
	hcljson "github.com/hashicorp/hcl/v2/json"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/internal/syntax"
)

// Sensitive is the mark of a sensitive value (design 6.3). It goes with the
// value through every expression that uses it.
const Sensitive = "sensitive"

// Input is a value given for a variable and where it was written; Range is
// nil for a value given without a place, such as --var.
type Input struct {
	Value cty.Value
	Range *hcl.Range
}

// maxValuesBytes bounds a text of values: a server takes it from a
// request, which nothing else bounds.
const maxValuesBytes = 1 << 20

// ParseValuesJSON parses values given as one JSON object, a member per
// variable: what ParseValues gives of the same values written as HCL. A
// string is a string, never a template.
func ParseValuesJSON(filename string, src []byte) (map[string]Input, hcl.Diagnostics) {
	if len(src) > maxValuesBytes {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Values too large",
			Detail: fmt.Sprintf("%s has %d bytes, more than the %d values may have.", filename, len(src), maxValuesBytes),
		}}
	}

	if err := syntax.CheckJSON(src); err != nil {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Values too complex", Detail: err.Error()}}
	}

	if trimmed := bytes.TrimSpace(src); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Values not an object",
			Detail: filename + " is not a JSON object: the values are an object with a member per variable.",
		}}
	}

	file, diags := hcljson.Parse(src, filename)
	if diags.HasErrors() {
		return nil, diags
	}

	return attributeInputs(file.Body, diags)
}

// ParseValues parses a file of values (a *.vars.hcl): one attribute per
// variable, each a constant expression.
func ParseValues(filename string, src []byte) (map[string]Input, hcl.Diagnostics) {
	if len(src) > maxValuesBytes {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Values too large",
			Detail: fmt.Sprintf("%s has %d bytes, more than the %d values may have.", filename, len(src), maxValuesBytes),
		}}
	}

	file, diags := syntax.ParseConfig(src, filename)
	if diags.HasErrors() {
		return nil, diags
	}

	return attributeInputs(file.Body, diags)
}

// attributeInputs are the attributes of a body of values, each a constant.
func attributeInputs(body hcl.Body, diags hcl.Diagnostics) (map[string]Input, hcl.Diagnostics) {
	attrs, moreDiags := body.JustAttributes()
	diags = append(diags, moreDiags...)
	inputs := make(map[string]Input, len(attrs))

	for name, attr := range attrs {
		v, moreDiags := attr.Expr.Value(nil)
		diags = append(diags, moreDiags...)

		if !moreDiags.HasErrors() {
			inputs[name] = Input{Value: v, Range: attr.Expr.Range().Ptr()}
		}
	}

	return inputs, diags
}

// Variables gives every variable of m its value: the input, else the
// default. It checks the values against their types and validations, then
// the rules, and returns the object var.* of an evaluation. A sensitive
// value carries the mark Sensitive.
func Variables(m *lang.Module, inputs map[string]Input) (cty.Value, hcl.Diagnostics) {
	diags := undeclared(m, inputs)
	values := make(map[string]cty.Value, len(m.Variables))

	for _, v := range m.Variables {
		val, moreDiags := assign(v, inputs)
		diags = append(diags, moreDiags...)
		values[v.Name] = val
	}

	vars := cty.ObjectVal(values)
	if diags.HasErrors() {
		return vars, diags
	}

	return vars, append(diags, checkRules(m.Rules, vars)...)
}

// undeclared reports the inputs for which m declares no variable.
func undeclared(m *lang.Module, inputs map[string]Input) hcl.Diagnostics {
	names := make([]string, 0, len(m.Variables))
	for _, v := range m.Variables {
		names = append(names, v.Name)
	}

	given := make([]string, 0, len(inputs))
	for name := range inputs {
		given = append(given, name)
	}

	sort.Strings(given)

	var diags hcl.Diagnostics

	for _, name := range given {
		if slices.Contains(names, name) {
			continue
		}

		detail := fmt.Sprintf("The module %q has no variable %q.", m.Metadata.Name, name)
		if hint := closest(name, names); hint != "" {
			detail += fmt.Sprintf(" Did you mean %q?", hint)
		}

		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Undeclared variable", Detail: detail, Subject: inputs[name].Range,
		})
	}

	return diags
}

// closest returns the name nearest to name, if one is near enough.
func closest(name string, names []string) string {
	best, bestDist := "", 3

	for _, n := range names {
		if d := levenshtein.Distance(name, n, nil); d < bestDist {
			best, bestDist = n, d
		}
	}

	return best
}

// assign returns the value of v: converted to its type and checked.
func assign(v *lang.Variable, inputs map[string]Input) (cty.Value, hcl.Diagnostics) {
	in, given := inputs[v.Name]
	rng := v.DeclRange

	switch {
	case given:
		if in.Range != nil {
			rng = *in.Range
		}
	case v.Default != cty.NilVal:
		return mark(v, v.Default), check(v, v.Default, rng)
	default:
		return cty.UnknownVal(v.Type), hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "No value for a variable",
			Detail:   fmt.Sprintf("The variable %q has no default; give it a value.", v.Name),
			Subject:  v.DeclRange.Ptr(),
		}}
	}

	val := in.Value

	// Before the value is converted and shown in a message: a number that is
	// given for a string is written there. A value of an HCL text is bounded
	// as it is read; this one may be of JSON, or computed by a stack.
	if err := walkNumbers(val, digitsError); err != nil {
		return cty.UnknownVal(v.Type), hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  summaryInvalidValue,
			Detail:   fmt.Sprintf("var.%s: %s.", v.Name, err),
			Subject:  rng.Ptr(),
		}}
	}

	if v.Defaults != nil {
		val = v.Defaults.Apply(val)
	}

	converted, err := convert.Convert(val, v.Type)
	if err != nil {
		return cty.UnknownVal(v.Type), hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  summaryInvalidValue,
			Detail:   fmt.Sprintf("var.%s must be a %s: %s.", v.Name, v.Type.FriendlyName(), err),
			Subject:  rng.Ptr(),
		}}
	}

	return mark(v, converted), check(v, converted, rng)
}

func mark(v *lang.Variable, val cty.Value) cty.Value {
	if v.Sensitive {
		return val.Mark(Sensitive)
	}

	return val
}

func check(v *lang.Variable, val cty.Value, rng hcl.Range) hcl.Diagnostics {
	k := &checker{rng: rng, sensitive: v.Sensitive}
	k.check(v.Validation, val, "var."+v.Name)

	return k.diags
}

// checkRules evaluates the conditions of the rules over vars.
func checkRules(rules []*lang.Rule, vars cty.Value) hcl.Diagnostics {
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{varRoot: vars},
		Functions: Functions(),
	}

	var diags hcl.Diagnostics

	for _, r := range rules {
		v, moreDiags := r.Condition.Value(ctx)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		v, _ = v.Unmark()
		if v.IsNull() || !v.IsKnown() || v.Type() != cty.Bool {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Invalid rule",
				Detail:  fmt.Sprintf("The condition of the rule %q must be true or false.", r.Name),
				Subject: r.Condition.Range().Ptr(),
			})

			continue
		}

		if v.False() {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Rule not met: " + r.Name,
				Detail: r.ErrorMessage, Subject: r.Condition.Range().Ptr(),
			})
		}
	}

	return diags
}
