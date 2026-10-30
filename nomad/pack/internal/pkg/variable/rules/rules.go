// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package rules parses and evaluates the rule blocks of variables.hcl:
// conditions on the variables of a pack that one variable alone can't
// express (Norca extension).
//
//	rule "tls_mode" {
//	  condition     = !var.tls_required || contains(["mtls", "secret"], var.tls_mode)
//	  error_message = "tls_mode must be mtls or secret when tls_required is set."
//	}
//
// Conditions see the variables of their pack as var.<name> and the
// functions of job specifications plus regex, regexall, alltrue and
// anytrue; can(regex(...)) tests a pattern.
package rules

import (
	"errors"
	"fmt"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/nomad/jobspec2"
	"github.com/hashicorp/nomad/pack/sdk/pack/variables"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

// BlockType is the type of rule blocks in variables.hcl.
const BlockType = "rule"

// Rule is a rule block.
type Rule struct {
	Name         string
	Condition    hcl.Expression
	ErrorMessage hcl.Expression
	DeclRange    hcl.Range
}

var fileSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: BlockType, LabelNames: []string{"name"}}},
}

var ruleSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "condition", Required: true},
		{Name: "error_message", Required: true},
	},
}

// Parse returns the rule blocks of a variables file; other blocks are
// left to the variable parser.
func Parse(filename string, content []byte) ([]*Rule, hcl.Diagnostics) {
	file, diags := hclparse.NewParser().ParseHCL(content, filename)
	if diags.HasErrors() {
		return nil, diags
	}
	body, _, partDiags := file.Body.PartialContent(fileSchema)
	diags = diags.Extend(partDiags)
	if body == nil {
		return nil, diags
	}
	var out []*Rule
	seen := map[string]bool{}
	for _, b := range body.Blocks {
		name := b.Labels[0]
		if !hclsyntax.ValidIdentifier(name) {
			diags = diags.Append(&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Invalid rule name",
				Detail: "A rule name must be a valid identifier.", Subject: b.LabelRanges[0].Ptr()})
			continue
		}
		if seen[name] {
			diags = diags.Append(&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Duplicate rule",
				Detail: fmt.Sprintf("A rule named %q is already declared.", name), Subject: b.DefRange.Ptr()})
			continue
		}
		seen[name] = true
		content, ruleDiags := b.Body.Content(ruleSchema)
		diags = diags.Extend(ruleDiags)
		if ruleDiags.HasErrors() {
			continue
		}
		r := &Rule{
			Name:         name,
			Condition:    content.Attributes["condition"].Expr,
			ErrorMessage: content.Attributes["error_message"].Expr,
			DeclRange:    b.DefRange,
		}
		diags = diags.Extend(checkReferences(r.Condition))
		diags = diags.Extend(checkReferences(r.ErrorMessage))
		out = append(out, r)
	}
	return out, diags
}

// checkReferences allows only var.<name> references.
func checkReferences(expr hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, t := range expr.Variables() {
		if t.RootName() != "var" || len(t) < 2 {
			diags = diags.Append(&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Invalid reference in rule",
				Detail: "A rule can only refer to the variables of its pack, as var.<name>.", Subject: t.SourceRange().Ptr()})
		}
	}
	return diags
}

// Functions are the functions of rule expressions.
func Functions() map[string]function.Function {
	funcs := jobspec2.Functions("", false)
	funcs["regex"] = stdlib.RegexFunc
	funcs["regexall"] = stdlib.RegexAllFunc
	funcs["alltrue"] = allTrue
	funcs["anytrue"] = anyTrue
	return funcs
}

var allTrue = boolListFunc(true)
var anyTrue = boolListFunc(false)

// boolListFunc returns alltrue (all) or anytrue (!all) of a list of bools.
func boolListFunc(all bool) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "list", Type: cty.List(cty.Bool)}},
		Type:   function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			for it := args[0].ElementIterator(); it.Next(); {
				_, v := it.Element()
				if v.IsNull() || !v.IsKnown() {
					return cty.False, nil
				}
				if v.True() != all {
					return cty.BoolVal(!all), nil
				}
			}
			return cty.BoolVal(all), nil
		},
	})
}

// Evaluate evaluates the rules against the values of the variables of
// their pack. It returns the messages of the failed rules, or the problems
// of rules that can't be evaluated.
func Evaluate(rules []*Rule, vars map[variables.ID]*variables.Variable) error {
	if len(rules) == 0 {
		return nil
	}
	values := make(map[string]cty.Value, len(vars))
	for id, v := range vars {
		val := v.Value
		if val == cty.NilVal {
			val = cty.NullVal(cty.DynamicPseudoType)
		}
		values[string(id)] = val
	}
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{"var": cty.ObjectVal(values)},
		Functions: Functions(),
	}
	var errs []error
	for _, r := range rules {
		cond, diags := r.Condition.Value(ctx)
		if diags.HasErrors() {
			errs = append(errs, fmt.Errorf("rule %q: %s", r.Name, diags.Error()))
			continue
		}
		if cond.IsNull() || !cond.IsKnown() || cond.Type() != cty.Bool {
			errs = append(errs, fmt.Errorf("rule %q: the condition must be true or false", r.Name))
			continue
		}
		if cond.True() {
			continue
		}
		msg, diags := r.ErrorMessage.Value(ctx)
		if diags.HasErrors() || msg.IsNull() || msg.Type() != cty.String {
			errs = append(errs, fmt.Errorf("rule %q failed", r.Name))
			continue
		}
		errs = append(errs, fmt.Errorf("%s (rule %q)", msg.AsString(), r.Name))
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errors.Join(errs...)
}
