// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/internal/syntax"
)

// Test is a test block of a tests/*.riggertest.hcl file (design 7.4): the
// install, the values, and the assertions over what the module gives, or
// the error it must give instead.
type Test struct {
	Name      string
	Release   string
	Namespace string
	Region    string
	// Values is the object of the values; nil for none.
	Values hcl.Expression
	// ExpectError is a text the diagnostics of the evaluation must hold;
	// empty when the evaluation must succeed.
	ExpectError string
	Asserts     []Assert
	DeclRange   hcl.Range
}

// Assert is an assert block of a test.
type Assert struct {
	Condition    hcl.Expression
	ErrorMessage string
}

// testFile is the schema of a test file.
type testFile struct {
	Tests []*testBlock `hcl:"test,block"`
}

type testBlock struct {
	Name        string         `hcl:"name,label"`
	Release     *string        `hcl:"release,optional"`
	Namespace   *string        `hcl:"namespace,optional"`
	Region      *string        `hcl:"region,optional"`
	Values      hcl.Expression `hcl:"values,optional"`
	ExpectError *string        `hcl:"expect_error,optional"`
	Asserts     []*assertBlock `hcl:"assert,block"`
	Body        hcl.Body       `hcl:",body"`
}

type assertBlock struct {
	Condition    hcl.Expression `hcl:"condition"`
	ErrorMessage string         `hcl:"error_message"`
}

// ParseTests parses a test file.
func ParseTests(filename string, src []byte) ([]*Test, hcl.Diagnostics) {
	file, diags := syntax.ParseConfig(src, filename)
	if diags.HasErrors() {
		return nil, diags
	}

	var f testFile
	if diags := gohcl.DecodeBody(file.Body, nil, &f); diags.HasErrors() {
		return nil, diags
	}

	var tests []*Test

	for _, b := range f.Tests {
		// The test is placed at the brace that opens its body.
		var rng hcl.Range
		if syntax, ok := b.Body.(*hclsyntax.Body); ok {
			rng = hcl.Range{Filename: filename, Start: syntax.SrcRange.Start, End: syntax.SrcRange.Start}
		}

		if slices.ContainsFunc(tests, func(t *Test) bool { return t.Name == b.Name }) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Duplicate test",
				Detail: fmt.Sprintf("The test %q is in this file twice.", b.Name), Subject: rng.Ptr(),
			})

			continue
		}

		t := &Test{
			Name: b.Name, Release: deref(b.Release), Namespace: deref(b.Namespace), Region: deref(b.Region),
			ExpectError: deref(b.ExpectError), DeclRange: rng,
		}

		if !isNullExpr(b.Values) {
			t.Values = b.Values
		}

		for _, a := range b.Asserts {
			t.Asserts = append(t.Asserts, Assert{Condition: a.Condition, ErrorMessage: a.ErrorMessage})
		}

		if t.ExpectError != "" && len(t.Asserts) > 0 {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Asserts of a test that expects an error",
				Detail: "A test expects an error or asserts over the result, not both.", Subject: rng.Ptr(),
			})
		}

		tests = append(tests, t)
	}

	return tests, diags
}

// isNullExpr reports whether an optional expression was left out.
func isNullExpr(expr hcl.Expression) bool {
	if expr == nil {
		return true
	}

	v, diags := expr.Value(nil)

	return !diags.HasErrors() && v.IsNull()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// Outcome is the result of a test.
type Outcome struct {
	// Failures say why the test failed; none when it passed.
	Failures []string
	// Diagnostics are those of an evaluation that failed unexpectedly, or
	// of an assertion that could not be evaluated.
	Diagnostics hcl.Diagnostics
}

// Passed reports whether the test passed.
func (o Outcome) Passed() bool {
	return len(o.Failures) == 0
}

// RunTest evaluates m with the values of t and checks its assertions.
func RunTest(ctx context.Context, m *lang.Module, t *Test, cfg Config) Outcome {
	facts := Facts{
		Release: or(t.Release, m.Metadata.Name), Namespace: or(t.Namespace, "default"), Region: or(t.Region, "global"),
		ModuleName: m.Metadata.Name, ModuleVersion: m.Metadata.Version,
	}

	vars, res, diags := testEvaluate(ctx, m, t, facts, cfg)

	if t.ExpectError != "" {
		switch {
		case !diags.HasErrors():
			return Outcome{Failures: []string{fmt.Sprintf("expected an error containing %q; the module evaluated", t.ExpectError)}}
		case !strings.Contains(diags.Error(), t.ExpectError):
			return Outcome{
				Failures:    []string{fmt.Sprintf("expected an error containing %q; got other errors", t.ExpectError)},
				Diagnostics: diags,
			}
		default:
			return Outcome{}
		}
	}

	if diags.HasErrors() {
		return Outcome{Failures: []string{"the module did not evaluate"}, Diagnostics: diags}
	}

	return checkAsserts(m, t, vars, facts, res, cfg)
}

// testEvaluate gives the variables their values and evaluates the module.
func testEvaluate(ctx context.Context, m *lang.Module, t *Test, facts Facts, cfg Config) (cty.Value, *Result, hcl.Diagnostics) {
	inputs, diags := testInputs(t)
	if diags.HasErrors() {
		return cty.NilVal, nil, diags
	}

	vars, diags := Variables(m, inputs)
	if diags.HasErrors() {
		return cty.NilVal, nil, diags
	}

	cfg.Test = true
	res, diags := Evaluate(ctx, m, vars, facts, cfg)

	return vars, res, diags
}

// testInputs evaluates the values of a test: an object, by variable.
func testInputs(t *Test) (map[string]Input, hcl.Diagnostics) {
	if t.Values == nil {
		return nil, nil
	}

	v, diags := t.Values.Value(&hcl.EvalContext{Functions: Functions()})
	if diags.HasErrors() {
		return nil, diags
	}

	if v.IsNull() || !v.IsWhollyKnown() || (!v.Type().IsObjectType() && !v.Type().IsMapType()) {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Invalid values",
			Detail: "The values of a test are an object: one attribute per variable.", Subject: t.Values.Range().Ptr(),
		}}
	}

	inputs := map[string]Input{}
	rng := t.Values.Range()

	for name, value := range v.AsValueMap() {
		inputs[name] = Input{Value: value, Range: &rng}
	}

	return inputs, nil
}

// checkAsserts evaluates the assertions of a test.
func checkAsserts(m *lang.Module, t *Test, vars cty.Value, facts Facts, res *Result, cfg Config) Outcome {
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			varRoot:    asLiterals(vars),
			riggerRoot: facts.value(),
			"resource": asLiterals(resourceObject(res.Resources)),
			"output":   asLiterals(res.OutputValues),
		},
		Functions: moduleFunctions(m.FS, cfg.FileBytes),
	}

	var out Outcome

	for _, a := range t.Asserts {
		v, diags := a.Condition.Value(ctx)
		if diags.HasErrors() {
			out.Failures = append(out.Failures, fmt.Sprintf("%s: the condition could not be evaluated", a.Condition.Range()))
			out.Diagnostics = append(out.Diagnostics, diags...)

			continue
		}

		v, _ = v.Unmark()

		switch {
		case v.IsNull() || !v.IsKnown() || v.Type() != cty.Bool:
			out.Failures = append(out.Failures, fmt.Sprintf("%s: the condition is not true or false", a.Condition.Range()))
		case v.False():
			out.Failures = append(out.Failures, fmt.Sprintf("%s: %s", a.Condition.Range(), a.ErrorMessage))
		}
	}

	return out
}

// resourceObject returns the object resource.* of a test: by block type
// (job, policy, variable, ...), then by name. A resource with count is a
// list, one with for_each an object by key. Every block type is there, so
// that length() and keys() read a type without resources.
func resourceObject(rs []Resource) cty.Value {
	byBlock := map[string]map[string][]Resource{}
	for _, b := range lang.ResourceBlocks() {
		byBlock[b] = map[string][]Resource{}
	}

	for _, r := range rs {
		if byBlock[r.Block] == nil {
			byBlock[r.Block] = map[string][]Resource{}
		}

		byBlock[r.Block][r.Base] = append(byBlock[r.Block][r.Base], r)
	}

	blocks := map[string]cty.Value{}

	for block, bases := range byBlock {
		values := map[string]cty.Value{}
		for base, group := range bases {
			values[base] = instancesValue(group)
		}

		blocks[block] = cty.ObjectVal(values)
	}

	return cty.ObjectVal(blocks)
}

// instancesValue returns the value of the copies of one resource.
func instancesValue(group []Resource) cty.Value {
	first := group[0]

	switch {
	case first.Key == cty.NilVal:
		return first.Value
	case first.Key.Type() == cty.Number:
		values := make([]cty.Value, len(group))

		for _, r := range group {
			i, _ := r.Key.AsBigFloat().Int64()
			values[i] = r.Value
		}

		return cty.TupleVal(values)
	default:
		values := map[string]cty.Value{}
		for _, r := range group {
			values[r.Key.AsString()] = r.Value
		}

		return cty.ObjectVal(values)
	}
}

// asLiterals gives lists and sets as tuples and maps as objects, the types
// of the literals an assertion compares with: ["a", "b"] is a tuple, and
// a list of the same strings is not equal to it.
//
// The walk is its own, top down: cty.Transform puts the changed items back
// into a collection of their old kind, and a list takes items of one type
// only, which the tuples of lists of different lengths are not.
func asLiterals(v cty.Value) cty.Value {
	inner, marks := v.Unmark()
	if inner.IsNull() || !inner.IsKnown() {
		return v
	}

	ty := inner.Type()

	switch {
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		items := make([]cty.Value, 0, inner.LengthInt())

		for it := inner.ElementIterator(); it.Next(); {
			_, item := it.Element()
			items = append(items, asLiterals(item))
		}

		return cty.TupleVal(items).WithMarks(marks)
	case ty.IsMapType() || ty.IsObjectType():
		attrs := make(map[string]cty.Value, inner.LengthInt())

		for it := inner.ElementIterator(); it.Next(); {
			name, item := it.Element()
			attrs[name.AsString()] = asLiterals(item)
		}

		return cty.ObjectVal(attrs).WithMarks(marks)
	default:
		return v
	}
}

func or(s, def string) string {
	if s == "" {
		return def
	}

	return s
}
