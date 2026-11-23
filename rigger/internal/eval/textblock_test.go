// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// The rules block of an ACL policy is its rules as HCL text: in the order
// of the module, dynamic blocks expanded, without comments.
func TestTextBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, src, want, err string
	}{
		{
			name: "a block",
			src: `rules {
  # read everything
  namespace "*" {
    capabilities = ["read-job"]
  }
  dynamic "namespace" {
    for_each = [var.ns]
    labels   = [namespace.value]
    content {
      variables {
        path "a/*" {
          capabilities = ["read"]
        }
      }
    }
  }
  node {
    policy = var.none
  }
}
`,
			want: "namespace \"*\" {\n  capabilities = [\"read-job\"]\n}\nnamespace \"apps\" {\n  variables {\n    path \"a/*\" {\n      capabilities = [\"read\"]\n    }\n  }\n}\nnode {\n}\n",
		},
		{name: "the attribute", src: "rules = \"node {}\"\n"},
		{name: "both", src: "rules = \"x\"\nrules {\n}\n", err: "one attribute holding the text, or one block"},
		{
			// $${ is how HCL writes the two characters ${ in a string.
			name: "a runtime reference",
			src:  "rules {\n  description = \"dc ${runtime(\"node.datacenter\")}\"\n}\n",
			want: "description = \"dc $${node.datacenter}\"\n",
		},
		{
			name: "a reference only a task template resolves",
			src:  "rules {\n  description = secret_ref(\"p\", \"i\")\n}\n",
			err:  "p.hcl:2,17-37: Value not encoded; The value of description could not be encoded: secret_ref: can be used only in the data of a task template.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, diags := hclsyntax.ParseConfig([]byte(tt.src), "p.hcl", hcl.InitialPos)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			ctx := &hcl.EvalContext{
				Variables: map[string]cty.Value{
					"var": cty.ObjectVal(map[string]cty.Value{"ns": cty.StringVal("apps"), "none": cty.NullVal(cty.String)}),
				},
				Functions: moduleFunctions(fstest.MapFS{}, 0),
			}

			in := cty.ObjectVal(map[string]cty.Value{"rules": cty.StringVal("node {}"), "name": cty.StringVal("p")})

			out, diags := textBlock(f.Body, "rules", in, ctx)
			if tt.err != "" {
				if !strings.Contains(diags.Error(), tt.err) {
					t.Fatalf("diagnostics = %v, want %q", diags, tt.err)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			want := tt.want
			if want == "" {
				want = "node {}"
			}

			if got := out.GetAttr("rules").AsString(); got != want {
				t.Errorf("rules = %q, want %q", got, want)
			}
		})
	}
}
