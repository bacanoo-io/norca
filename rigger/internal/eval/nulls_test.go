// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// dropNulls leaves out the attributes whose value is null, in nested blocks
// too, and keeps those it can't evaluate.
func TestDropNulls(t *testing.T) {
	t.Parallel()

	src := "a = null\nb = 1\nc = x.unknown\ninner {\n  d = null\n  e = \"e\"\n}\n"

	f, diags := hclsyntax.ParseConfig([]byte(src), "t.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	body := dropNulls(f.Body, &hcl.EvalContext{})

	content, rest, diags := body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: "a"}, {Name: "b"}},
		Blocks:     []hcl.BlockHeaderSchema{{Type: "inner"}},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if _, ok := content.Attributes["a"]; ok || content.Attributes["b"] == nil {
		t.Errorf("attributes = %v, want b only", content.Attributes)
	}

	if attrs, _ := rest.JustAttributes(); attrs["c"] == nil {
		t.Errorf("rest = %v, want c kept", attrs)
	}

	inner, _ := content.Blocks[0].Body.JustAttributes()
	if _, ok := inner["d"]; ok || inner["e"] == nil {
		t.Errorf("inner = %v, want e only", inner)
	}
}
