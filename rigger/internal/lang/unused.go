// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// UnusedVariables warns of the variables of m that change nothing: no
// local, resource, check, output or call of a child module reads them
// (design 14.1). A rule only checks values, so a variable that only rules
// read is unused too.
func UnusedVariables(m *Module) hcl.Diagnostics {
	read := map[string]bool{}
	all := false

	visit := func(body hcl.Body) {
		syntax, ok := body.(*hclsyntax.Body)
		if !ok {
			return
		}

		_ = hclsyntax.VisitAll(syntax, func(n hclsyntax.Node) hcl.Diagnostics {
			expr, ok := n.(*hclsyntax.ScopeTraversalExpr)
			if !ok || expr.Traversal.RootName() != "var" {
				return nil
			}

			if len(expr.Traversal) < 2 {
				all = true // var as a whole

				return nil
			}

			if attr, ok := expr.Traversal[1].(hcl.TraverseAttr); ok {
				read[attr.Name] = true
			}

			return nil
		})
	}

	for _, blocks := range [][]*hcl.Block{m.Locals, m.Checks, m.Outputs, m.Calls} {
		for _, b := range blocks {
			visit(b.Body)
		}
	}

	for _, r := range m.Resources {
		visit(r.Block.Body)
	}

	if all {
		return nil
	}

	var diags hcl.Diagnostics

	for _, v := range m.Variables {
		if read[v.Name] {
			continue
		}

		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Unused variable",
			Detail: fmt.Sprintf("var.%s is read by no local, resource, check, output or child module, so its value "+
				"changes nothing. Use it, or remove it.", v.Name),
			Subject: v.DeclRange.Ptr(),
		})
	}

	return diags
}
