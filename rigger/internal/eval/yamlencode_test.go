// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/runref"
)

// A list that ends with the addresses of a service is written on one line,
// and so is every item of it: an object as a mapping in braces, with its
// attributes.
func TestYAMLEncode_InlineObjects(t *testing.T) {
	t.Parallel()

	// The addresses as the encoder writes them after an item of the list.
	services := runref.Part{Kind: runref.KindServices, Name: "s"}
	after := runref.Part{Kind: runref.KindServices, Name: "s", Quote: runref.QuoteConf, Sep: ", ", Lead: ", "}.String()

	list := func(items ...cty.Value) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"a": cty.TupleVal(append(items, cty.StringVal(services.String())))})
	}

	tests := []struct {
		name     string
		value    cty.Value
		expected string
	}{
		{
			name: "an object",
			value: list(cty.ObjectVal(map[string]cty.Value{
				"host": cty.StringVal("h"), "port": cty.NumberIntVal(1),
			})),
			expected: `a: [{host: "h", port: 1}` + after + "]\n",
		},
		{
			name: "objects and lists in an object",
			value: list(cty.ObjectVal(map[string]cty.Value{
				"b":      cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.ObjectVal(map[string]cty.Value{"c": cty.True})}),
				"my key": cty.NullVal(cty.String),
				"empty":  cty.EmptyObjectVal,
				"m":      cty.MapVal(map[string]cty.Value{"k": cty.StringVal("v")}),
			})),
			expected: `a: [{b: [1, {c: true}], empty: {}, m: {k: "v"}, "my key": null}` + after + "]\n",
		},
		{
			name:     "an empty object",
			value:    list(cty.EmptyObjectVal),
			expected: "a: [{}" + after + "]\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := yamlEncode.Call([]cty.Value{tt.value})
			if err != nil {
				t.Fatal(err)
			}

			if got := out.AsString(); got != tt.expected {
				t.Errorf("got\n%s\nwant\n%s", got, tt.expected)
			}
		})
	}
}
