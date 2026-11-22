// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// renderBlock writes the block as it evaluates: attributes in their order,
// nulls left out, dynamic blocks expanded in their order, labels kept.
func TestRenderBlock(t *testing.T) {
	t.Parallel()

	src := `kind "dns-record" {
  enabled = var.on
  name    = "DNSRecord"
  group   = "norca.io"
  extra   = var.none

  columns {
    column "Name" { path = "spec.name" }
    dynamic "column" {
      for_each = ["Zone", "Ready"]
      labels   = [column.value]

      content {
        path = "status.${lower(column.value)}"
      }
    }
  }
}
`

	f, diags := hclsyntax.ParseConfig([]byte(src), "k.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{"var": cty.ObjectVal(map[string]cty.Value{"on": cty.True, "none": cty.NullVal(cty.String)})},
		Functions: Functions(),
	}

	syntax, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		t.Fatal("not a native body")
	}

	block := syntax.Blocks[0].AsHCLBlock()

	out, diags := renderBlock(block, "dns-record", ctx, metaNames, nil)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	got := string(out)
	for _, want := range []string{`kind "dns-record" {`, `name  = "DNSRecord"`} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}

	order := []string{`name`, `group`, `column "Name"`, `column "Zone"`, `"status.zone"`, `column "Ready"`, `"status.ready"`}
	last := -1

	for _, s := range order {
		i := strings.Index(got, s)
		if i <= last {
			t.Fatalf("%q out of order in:\n%s", s, got)
		}

		last = i
	}

	for _, unwanted := range []string{"enabled", "extra", "dynamic", "for_each"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q in:\n%s", unwanted, got)
		}
	}
}
