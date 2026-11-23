// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
)

// A template that is one interpolation and nothing else gives the value of
// the interpolation, not its text. templatefile returns a string: a number
// or a bool is its string, as inside a longer template, and what has no
// string is an error that says so.
func TestEvaluate_TemplateOfOneInterpolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		x        string
		rules    string // in the spec of the policy
		expected string // in the diagnostics
	}{
		{name: "a string", x: `"a"`, rules: `"rules":"a"`},
		{name: "a number", x: "5", rules: `"rules":"5"`},
		{name: "a fraction", x: "0.5", rules: `"rules":"0.5"`},
		{name: "a bool", x: "true", rules: `"rules":"true"`},
		{
			name: "a null", x: "null",
			expected: `x.acl.hcl:2,11-24: Error in function call; Call to function "templatefile" failed: the template files/one.tpl gives null, not a string.`,
		},
		{
			name: "a list", x: `["a"]`,
			expected: `x.acl.hcl:2,11-24: Error in function call; Call to function "templatefile" failed: the template files/one.tpl gives a tuple, not a string.`,
		},
		{
			name: "an object", x: `{ a = 1 }`,
			expected: `Call to function "templatefile" failed: the template files/one.tpl gives an object, not a string.`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, diags := evaluate(t, module(map[string]string{
				"files/one.tpl": "${x}",
				xACL:            "policy \"x\" {\n  rules = templatefile(\"files/one.tpl\", { x = " + tt.x + " })\n}\n",
			}), ``)

			if tt.expected != "" {
				if got := diags.Error(); len(diags) != 1 || !strings.Contains(got, tt.expected) {
					t.Errorf("diagnostics = %q, want the one that contains %q", got, tt.expected)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if got := string(res.Resources[0].Spec); !strings.Contains(got, tt.rules) {
				t.Errorf("spec = %s, want it to contain %s", got, tt.rules)
			}
		})
	}
}
