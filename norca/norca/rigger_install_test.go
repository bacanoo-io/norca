// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
)

// The text of diagnostics names the file of a diagnostic that has one, and
// none of an error of the servers: before, the operation of a stack whose
// release failed ended with "<nil>: Apply stopped; ..." (norca-deployer tree
// e2e, 2026-11-28).
func TestDiagsText(t *testing.T) {
	ci.Parallel(t)

	stopped := &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Apply stopped", Detail: `release "web" failed.`}
	inFile := &hcl.Diagnostic{
		Severity: hcl.DiagError, Summary: "Unsupported argument", Detail: `An argument named "x" is not expected here.`,
		Subject: &hcl.Range{Filename: "stack.hcl", Start: hcl.Pos{Line: 3, Column: 5}, End: hcl.Pos{Line: 3, Column: 6}},
	}
	warning := &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "Deprecated"}

	tests := []struct {
		name  string
		diags hcl.Diagnostics
		want  string
	}{
		{name: "no subject", diags: hcl.Diagnostics{stopped}, want: `Apply stopped; release "web" failed.`},
		{name: "a subject", diags: hcl.Diagnostics{inFile}, want: `stack.hcl:3,5-6: Unsupported argument; An argument named "x" is not expected here.`},
		{name: "warnings left out", diags: hcl.Diagnostics{warning, stopped, inFile}, want: `Apply stopped; release "web" failed., and 1 other diagnostic(s)`},
		{name: "only a warning", diags: hcl.Diagnostics{warning}, want: "Deprecated"},
		{name: "none", want: "no diagnostics"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.want, diagsText(tt.diags))
		})
	}
}
