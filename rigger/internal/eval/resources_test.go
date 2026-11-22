// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The items of a Variable are strings in Norca: a number or a bool becomes
// its string, as HCL converts a value for a string; a list, a map or an
// object is refused with the item named, when the module is evaluated, not
// by the server at apply.
func TestEvaluate_VariableItems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		items string
		spec  string // in the spec of the Variable
		err   string // in the diagnostics
	}{
		{name: "string", items: `name = "web"`, spec: `"items":{"name":"web"}`},
		{name: "number", items: `count = 3`, spec: `"items":{"count":"3"}`},
		{name: "fraction", items: `ratio = 0.5`, spec: `"items":{"ratio":"0.5"}`},
		{name: "bool", items: `tls = true`, spec: `"items":{"tls":"true"}`},
		{name: "number of a variable", items: `count = var.count`, spec: `"items":{"count":"2"}`},
		{name: "list", items: `hosts = ["a", "b"]`, err: `The item hosts of the Variable is a tuple`},
		{name: "object", items: `db = { host = "a" }`, err: `The item db of the Variable is an object`},
		// null is a value not given: the item is left out, as any attribute
		// set to null.
		{name: "null", items: "empty = null\n    name = \"web\"", spec: `"items":{"name":"web"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{
				"metadata.hcl":  {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
				"variables.hcl": {Data: []byte("variable \"count\" {\n  type    = number\n  default = 2\n}\n")},
				"app.var.hcl":   {Data: []byte("variable \"app\" {\n  path = \"apps/app\"\n\n  items {\n    " + tt.items + "\n  }\n}\n")},
			}

			res, diags := evaluate(t, fsys, "")

			if tt.err != "" {
				if !strings.Contains(diags.Error(), tt.err) {
					t.Fatalf("diagnostics %q, want %q", diags.Error(), tt.err)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if got := string(res.Resources[0].Spec); !strings.Contains(got, tt.spec) {
				t.Errorf("spec %s, want %s", got, tt.spec)
			}
		})
	}
}
