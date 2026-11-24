// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// varReads are the variables the expressions of a module read.
type varReads struct {
	names map[string]bool
	// all says that an expression reads var as a whole, so every variable
	// is read.
	all bool
}

// node records the variables the expressions under n read.
func (r *varReads) node(n hclsyntax.Node) {
	_ = hclsyntax.VisitAll(n, func(n hclsyntax.Node) hcl.Diagnostics {
		expr, ok := n.(*hclsyntax.ScopeTraversalExpr)
		if !ok || expr.Traversal.RootName() != "var" {
			return nil
		}

		if len(expr.Traversal) < 2 {
			r.all = true

			return nil
		}

		if attr, ok := expr.Traversal[1].(hcl.TraverseAttr); ok {
			r.names[attr.Name] = true
		}

		return nil
	})
}

// body records the variables the expressions of a body read.
func (r *varReads) body(body hcl.Body) {
	if syntax, ok := body.(*hclsyntax.Body); ok {
		r.node(syntax)
	}
}

// UnusedVariables warns of the variables of m that change nothing: no
// local, rule, resource, check, output or call of a child module reads them
// (design 14.1). A variable that only a rule reads is used: its value
// decides whether the module evaluates, as a gate the installer sets does
// (the acceptance of a license, the consent to a risky setting).
func UnusedVariables(m *Module) hcl.Diagnostics {
	reads := &varReads{names: map[string]bool{}}

	for _, blocks := range [][]*hcl.Block{m.Locals, m.Checks, m.Outputs, m.Calls} {
		for _, b := range blocks {
			reads.body(b.Body)
		}
	}

	for _, r := range m.Resources {
		reads.body(r.Block.Body)
	}

	for _, r := range m.Rules {
		if expr, ok := r.Condition.(hclsyntax.Expression); ok {
			reads.node(expr)
		}
	}

	if reads.all {
		return nil
	}

	var diags hcl.Diagnostics

	for _, v := range m.Variables {
		if reads.names[v.Name] {
			continue
		}

		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Unused variable",
			Detail: fmt.Sprintf("var.%s is read by no local, rule, resource, check, output or child module, so its value "+
				"changes nothing. Use it, or remove it.", v.Name),
			Subject: v.DeclRange.Ptr(),
		})
	}

	return diags
}
