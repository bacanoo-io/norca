// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"strings"
	"testing"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// Values as JSON, as the form of the UI sends them, are the values the
// same file of HCL gives: the same values, so the same digest of a release.
func TestParseValuesJSON(t *testing.T) {
	t.Parallel()

	const (
		asHCL  = "count = 2\nname = \"web\"\ntags = [\"a\", \"b\"]\nlimits = {\n  cpu = 500\n  on  = true\n}\ntemplate = \"$${x}\"\nnothing = null\n"
		asJSON = `{"count": 2, "name": "web", "tags": ["a", "b"], "limits": {"cpu": 500, "on": true}, "template": "${x}", "nothing": null}`
	)

	fromHCL, diags := rigger.ParseValues("values.vars.hcl", []byte(asHCL))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	fromJSON, diags := rigger.ParseValuesJSON("values.json", []byte(asJSON))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if len(fromJSON) != len(fromHCL) {
		t.Fatalf("ParseValuesJSON() has %d values, the HCL %d", len(fromJSON), len(fromHCL))
	}

	for name, want := range fromHCL {
		got, ok := fromJSON[name]
		if !ok {
			t.Errorf("%s is missing", name)

			continue
		}

		if !got.Value.RawEquals(want.Value) {
			t.Errorf("%s = %#v, the HCL gives %#v", name, got.Value, want.Value)
		}
	}
}

func TestParseValuesJSON_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, src, err string
	}{
		{name: "not an object", src: `["a"]`, err: "an object"},
		{name: "not JSON", src: `{"a": }`, err: "JSON value"},
		{name: "too deep", src: strings.Repeat(`{"a": `, 300) + "1" + strings.Repeat("}", 300), err: "nest"},
		{name: "too large", src: `{"a": "` + strings.Repeat("x", 2<<20) + `"}`, err: "too large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, diags := rigger.ParseValuesJSON("values.json", []byte(tt.src))
			if !diags.HasErrors() || !strings.Contains(diags.Error(), tt.err) {
				t.Errorf("ParseValuesJSON() = %v, want an error with %q", diags, tt.err)
			}
		})
	}
}
