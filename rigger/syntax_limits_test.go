// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// The messages of the bounds of a text.
const (
	tooDeep    = "nested more than 256 deep"
	tooComplex = "more than 20000 operators and brackets"
	tooLarge   = "more than the 1048576"
)

// deep is an expression nested past the bound, and harmless without it: a
// deeper one overflows the stack of the parser, which ends the process.
func deep() string {
	return strings.Repeat("[", 300) + strings.Repeat("]", 300)
}

// What the engine parses of others is bounded before the parser sees it: a
// stack overflow ends the process of a server, and no recover catches it.
func TestEngine_SyntaxLimits(t *testing.T) {
	t.Parallel()

	loadModule := func(t *testing.T, name, src string) hcl.Diagnostics {
		t.Helper()

		e := engine(t)

		m, diags := e.LoadModule(t.Context(), fstest.MapFS{
			metadataFile: {Data: []byte("module {\n  name    = \"m\"\n  version = \"0.1.0\"\n}\n")},
			name:         {Data: []byte(src)},
			"files/deep": {Data: []byte("${" + deep() + "}")},
			"files/json": {Data: []byte(deep())},
		})
		if diags.HasErrors() {
			return diags
		}

		_, diags = e.Evaluate(t.Context(), m, rigger.Install{Release: "m", Namespace: testNamespace, Region: "eu"})

		return diags
	}

	loadStack := func(t *testing.T, name, src string) hcl.Diagnostics {
		t.Helper()

		files := stackFiles(twoReleases)
		files["s/"+name] = &fstest.MapFile{Data: []byte(src)}

		_, diags := engine(t).LoadStack(t.Context(), files, "s")

		return diags
	}

	parseValues := func(t *testing.T, name, src string) hcl.Diagnostics {
		t.Helper()

		_, diags := rigger.ParseValues(name, []byte(src))

		return diags
	}

	const locals = "locals.hcl"

	tests := []struct {
		name     string
		load     func(t *testing.T, name, src string) hcl.Diagnostics
		file     string
		src      string
		expected string
	}{
		{name: "values", load: parseValues, file: "v.vars.hcl", src: "a = " + deep(), expected: tooDeep},
		{name: "values too large", load: parseValues, file: "v.vars.hcl", src: "# " + strings.Repeat("x", 1<<20), expected: tooLarge},
		{name: "module", load: loadModule, file: locals, src: "locals {\n  x = " + deep() + "\n}\n", expected: tooDeep},
		{
			name: "chain without brackets", load: loadModule, file: locals,
			src: "locals {\n  x = 0" + strings.Repeat("+0", 20001) + "\n}\n", expected: tooComplex,
		},
		{
			name: "template", load: loadModule, file: locals,
			src: "locals {\n  x = templatefile(\"files/deep\", {})\n}\n", expected: tooDeep,
		},
		{
			name: "jsondecode", load: loadModule, file: locals,
			src: "locals {\n  x = jsondecode(file(\"files/json\"))\n}\n", expected: tooDeep,
		},
		{name: "stack", load: loadStack, file: locals, src: "locals {\n  x = " + deep() + "\n}\n", expected: tooDeep},
		{name: "stack file too large", load: loadStack, file: locals, src: "# " + strings.Repeat("x", 1<<20), expected: tooLarge},
		{name: "lock", load: loadStack, file: "rigger.lock.hcl", src: "x = " + deep() + "\n", expected: tooDeep},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if diags := tt.load(t, tt.file, tt.src); !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %.220q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}

// The condition of a ui block is a string that FormSchema parses.
func TestModule_FormSchemaDeepShowIf(t *testing.T) {
	t.Parallel()

	cond := "on == " + strings.Repeat("(", 300) + "1" + strings.Repeat(")", 300)

	m, diags := engine(t).LoadModule(t.Context(), fstest.MapFS{
		metadataFile: {Data: []byte("module {\n  name    = \"m\"\n  version = \"0.1.0\"\n}\n")},
		variablesFile: {Data: []byte("variable \"on\" {\n  default = 1\n}\n\nvariable \"x\" {\n  default = 1\n\n  ui {\n    show_if = \"" +
			cond + "\"\n  }\n}\n")},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	props, ok := m.FormSchema()["properties"].(map[string]any)
	if !ok {
		t.Fatal("the schema has no properties")
	}

	x, ok := props["x"].(map[string]any)
	if !ok {
		t.Fatal("the schema has no property x")
	}

	if _, parsed := x["x-ui-show-if"]; parsed {
		t.Error("a condition nested 300 deep was parsed; it is kept as written")
	}
}
