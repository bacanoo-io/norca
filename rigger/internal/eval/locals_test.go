// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
)

// A value is evaluated after the values it reads, however it names them:
// as an attribute, as an index with the name, or the whole root, which
// reads every other value of it. The locals of the tests are named so that
// the order of the names is not the order of the references.
func TestEvaluate_ValueReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		locals   string
		output   string // the output local.a as JSON
		expected string // in the diagnostics
	}{
		{name: "an attribute", locals: "z = 1\n  a = local.z", output: "1"},
		{name: "an index with the name", locals: "z = 1\n  a = local[\"z\"]", output: "1"},
		{name: "an index with the name, then an attribute", locals: "z = { n = 1 }\n  a = local[\"z\"].n", output: "1"},
		{name: "the whole root", locals: "z = 1\n  y = 2\n  a = keys(local)", output: `["y","z"]`},
		{name: "a name that is computed", locals: "z = 1\n  y = \"z\"\n  a = local[local.y]", output: "1"},
		{name: "a for expression over the root", locals: "z = 1\n  y = 2\n  a = [for k, v in local : \"${k}=${v}\"]", output: `["y=2","z=1"]`},
		{
			name: "two values that read the whole root", locals: "a = keys(local)\n  b = keys(local)",
			expected: "Values refer to each other; The locals and modules form a cycle: local.a → local.b → local.a.",
		},
		{
			name: "a value that reads one that reads the whole root", locals: "a = keys(local)\n  b = local.a",
			expected: "Values refer to each other; The locals and modules form a cycle: local.a → local.b → local.a.",
		},
		{name: "a value that is not there", locals: "a = local[\"z\"]", expected: "Invalid index"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, diags := evaluate(t, module(map[string]string{
				localsHCL:  "locals {\n  " + tt.locals + "\n}\n",
				outputsHCL: "output \"a\" {\n  value = local.a\n}\n",
			}), ``)

			if tt.expected != "" {
				if !strings.Contains(diags.Error(), tt.expected) {
					t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if got := string(res.Outputs[0].Value); got != tt.output {
				t.Errorf("local.a = %s, want %s", got, tt.output)
			}
		})
	}
}
