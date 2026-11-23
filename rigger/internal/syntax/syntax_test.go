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

// A number literal is bounded by the digits it has written without its
// exponent: a few characters of exponent make a number that takes seconds
// and megabytes to write.
func TestParse_NumberLiterals(t *testing.T) {
	t.Parallel()

	digits := strings.Repeat("7", syntax.MaxNumberDigits)

	tests := []struct {
		name    string
		literal string
		refused bool
	}{
		{name: "a whole number", literal: "4222"},
		{name: "a fraction", literal: "12345.678"},
		{name: "the largest float64", literal: "1.7976931348623157e308"},
		{name: "the smallest float64", literal: "5e-324"},
		{name: "as many digits as the bound", literal: digits},
		{name: "as many digits after the point as the bound", literal: "0." + digits},
		{name: "zeros before as many digits as the bound", literal: "000" + digits},
		{name: "an exponent up to the bound", literal: "1e999"},
		{name: "an exponent down to the bound", literal: "1E-1000"},
		{name: "an exponent that moves the point inside the digits", literal: digits[1:] + ".5e-900"},
		{name: "one digit more than the bound", literal: digits + "7", refused: true},
		{name: "one digit more after the point", literal: "0." + digits + "7", refused: true},
		{name: "one digit more once the exponent moved the point", literal: digits + ".5e-900", refused: true},
		{name: "an exponent over the bound", literal: "1e1000", refused: true},
		{name: "an exponent with a sign over the bound", literal: "1E+1000", refused: true},
		{name: "an exponent under the bound", literal: "1e-1001", refused: true},
		{name: "a fraction with an exponent under the bound", literal: "0.5e-1000", refused: true},
		{name: "the number of the report", literal: "1e4000000", refused: true},
		{name: "an exponent no integer holds", literal: "1e99999999999999999999999", refused: true},
		{name: "zeros before an exponent over the bound", literal: "1e0000000001000", refused: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// In a file, in an expression and in a template.
			_, config := syntax.ParseConfig([]byte("a = [1, "+tt.literal+"]\n"), "x.hcl")
			_, expr := syntax.ParseExpression([]byte("f("+tt.literal+")"), "x.hcl")
			_, template := syntax.ParseTemplate([]byte("n=${"+tt.literal+"}"), "x.tpl")

			for where, diags := range map[string]hcl.Diagnostics{"a file": config, "an expression": expr, "a template": template} {
				if !tt.refused {
					if diags.HasErrors() {
						t.Errorf("in %s: %v, want it read", where, diags)
					}

					continue
				}

				const want = "The text can't be read: it has a number of more than 1000 digits, the limit of the engine."
				if len(diags) != 1 || diags[0].Summary != syntax.Summary || diags[0].Detail != want {
					t.Errorf("in %s: %v, want %q", where, diags, want)
				}
			}
		})
	}
}

// A text that only looks like a number is none: a string, a name.
func TestParse_NotNumberLiterals(t *testing.T) {
	t.Parallel()

	for _, src := range []string{"a = \"1e4000000\"\n", "a = e4000000\n", "a1e4000000 = 1\n", "a = <<E\n1e4000000\nE\n"} {
		if _, diags := syntax.ParseConfig([]byte(src), "x.hcl"); diags.HasErrors() {
			t.Errorf("%q: %v, want it read", src, diags)
		}
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
