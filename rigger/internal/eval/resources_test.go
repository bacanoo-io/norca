// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/resource"
)

// sourceDecoder is a decoder of the host that keeps the block it is given:
// the spec of an object is the text of its block, as a JSON string.
type sourceDecoder struct{}

func (sourceDecoder) Kinds() []resource.Kind {
	return []resource.Kind{resource.KindObject}
}

func (sourceDecoder) DecodeResource(_ context.Context, _ resource.Kind, _ string, src []byte) (json.RawMessage, hcl.Diagnostics) {
	data, err := json.Marshal(string(src))
	if err != nil {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Not encoded", Detail: err.Error()}}
	}

	return data, nil
}

// The block a decoder of the host is given has its runtime references
// resolved as outside a task template, as the value of a block the engine
// encodes itself has: the decoder does not know the form of a reference
// inside the engine. $${ is how HCL writes the two characters ${ in a
// string.
func TestEvaluate_DecodedRuntimeReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    string
		source   string // in the text the decoder is given
		expected string // in the diagnostics
	}{
		{name: "a whole value", value: `runtime_env("HOME")`, source: `a = "$${HOME}"`},
		{name: "a part of a string", value: `"${runtime("node.datacenter")}/x"`, source: `a = "$${node.datacenter}/x"`},
		{name: "an item of a list", value: `["a", runtime_env("HOME")]`, source: `a = ["a", "$${HOME}"]`},
		{name: "no reference", value: `"a"`, source: `a = "a"`},
		{
			name: "a reference only a task template resolves", value: `secret_ref("p", "i")`,
			expected: "x.object.hcl:3,9-29: Value not encoded; The value of a could not be encoded: secret_ref: can be used only in the data of a task template.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := load(t, module(map[string]string{xObject: "object \"x\" {\n  spec {\n    a = " + tt.value + "\n  }\n}\n"}))

			res, diags := eval.Evaluate(t.Context(), m, cty.EmptyObjectVal, facts, eval.Config{
				FileBytes: 1 << 20, Resources: 10, Decoder: sourceDecoder{},
			})

			if tt.expected != "" {
				if got := diags.Error(); len(diags) != 1 || !strings.Contains(got, tt.expected) {
					t.Errorf("diagnostics = %q, want the one that contains %q", got, tt.expected)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			var source string
			if err := json.Unmarshal(res.Resources[0].Spec, &source); err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(source, tt.source) {
				t.Errorf("the decoder was given\n%s\nwant it to contain %s", source, tt.source)
			}
		})
	}
}

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
