// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package syntax_test

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/syntax"
)

// Each shape deepens the parser or the evaluation of HCL; at a megabyte
// each overflows the stack without the bound.
func TestParseConfig_Hostile(t *testing.T) {
	t.Parallel()

	const n = 30000

	tests := []struct {
		name string
		src  string
	}{
		{name: "tuples", src: "a = " + strings.Repeat("[", n)},
		{name: "objects", src: "a = " + strings.Repeat("{a=", n)},
		{name: "parentheses", src: "a = " + strings.Repeat("(", n)},
		{name: "calls", src: "a = " + strings.Repeat("f(", n)},
		{name: "blocks", src: strings.Repeat("a {\n", n)},
		{name: "templates", src: "a = " + strings.Repeat(`"${`, n)},
		{name: "heredocs", src: "a = " + strings.Repeat("<<E\n${", n)},
		{name: "closers that close nothing", src: "a = " + strings.Repeat("([}", n)},
		{name: "not", src: "a = " + strings.Repeat("!", n) + "true"},
		{name: "negation", src: "a = " + strings.Repeat("- ", n) + "1"},
		{name: "conditionals", src: "a = " + strings.Repeat("true?1:", n) + "1"},
		{name: "sum", src: "a = 1" + strings.Repeat("+1", n)},
		{name: "indexes", src: "a = b" + strings.Repeat("[c]", n)},
		{name: "splats", src: "a = b" + strings.Repeat(".*", n)},
		{name: "directives", src: `a = "` + strings.Repeat("%{if true}", n) + `"`},
		{name: "values deep in many attributes", src: strings.Repeat("a = {b={c={d=1}}}\n", n)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, diags := syntax.ParseConfig([]byte(tt.src), "x.hcl")
			if f != nil || !diags.HasErrors() || diags[0].Summary != syntax.Summary {
				t.Errorf("ParseConfig() = %v, want it refused before the parser", diags)
			}
		})
	}
}

// At the bound a text still parses and evaluates.
func TestParseConfig_AtBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
	}{
		{name: "nesting", src: "a = " + strings.Repeat("[", syntax.MaxDepth) + strings.Repeat("]", syntax.MaxDepth)},
		{name: "chain", src: "a = 1" + strings.Repeat("+1", syntax.MaxOperators)},
		{name: "not", src: "a = " + strings.Repeat("!", syntax.MaxOperators) + "true"},
		{name: "conditionals", src: "a = " + strings.Repeat("false?1:", syntax.MaxOperators) + "1"},
		{name: "many attributes", src: attributes(5000)},
		{name: "a long text", src: "a = <<E\n" + strings.Repeat("a line (of text) [with] {signs} + - * ?\n", 20000) + "E\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, diags := syntax.ParseConfig([]byte(tt.src), "x.hcl")
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			attrs, diags := f.Body.JustAttributes()
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			var evalDiags hcl.Diagnostics

			for _, a := range attrs {
				_, d := a.Expr.Value(nil)
				evalDiags = append(evalDiags, d...)
			}

			if evalDiags.HasErrors() {
				t.Fatal(evalDiags)
			}
		})
	}
}

// attributes are n attributes with a small value.
func attributes(n int) string {
	var b strings.Builder

	for i := range n {
		b.WriteString("a")
		b.WriteString(strings.Repeat("a", i%7))
		b.WriteString("_")

		for d := i; d > 0; d /= 10 {
			b.WriteByte(byte('0' + d%10))
		}

		b.WriteString(" = [\"x\"]\n")
	}

	return b.String()
}

func TestCheckJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		src   string
		valid bool
	}{
		{name: "deep arrays", src: strings.Repeat("[", syntax.MaxDepth+1)},
		{name: "deep objects", src: strings.Repeat(`{"a":`, syntax.MaxDepth+1)},
		{name: "at the bound", src: strings.Repeat("[", syntax.MaxDepth) + strings.Repeat("]", syntax.MaxDepth), valid: true},
		{name: "brackets in a string", src: `"` + strings.Repeat("[", 1000) + `\"` + strings.Repeat("{", 1000) + `"`, valid: true},
		{name: "wide", src: "[" + strings.Repeat("[],", 5000) + "[]]", valid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := syntax.CheckJSON([]byte(tt.src)); (err == nil) != tt.valid {
				t.Errorf("CheckJSON() = %v, want valid = %v", err, tt.valid)
			}
		})
	}
}
