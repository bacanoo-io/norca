// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package syntax parses HCL that others wrote, within bounds. The parser
// and the evaluation of HCL recurse as deep as the text makes them, and a
// stack overflow ends the process of the host: no recover catches one. The
// bounds are checked on the tokens, whose scanner does not recurse.
package syntax

import (
	"errors"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// The bounds of one text.
const (
	// MaxDepth bounds the nesting of brackets, blocks and templates.
	MaxDepth = 256
	// MaxOperators bounds what deepens an expression, or the value it
	// gives, without nesting: operators, and the closing brackets of
	// indexes, tuples, objects and blocks. It is a bound on the whole text,
	// so no rule of where an expression ends has to be copied from the
	// parser.
	MaxOperators = 20000
)

// Summary is the summary of the diagnostic of a text over a bound.
const Summary = "Text too complex"

var (
	errDepth     = fmt.Errorf("it is nested more than %d deep, the limit of the engine", MaxDepth)
	errOperators = fmt.Errorf("it has more than %d operators and brackets, the limit of the engine", MaxOperators)
)

// ParseConfig is hclsyntax.ParseConfig within the bounds. A text over one
// gives no file.
func ParseConfig(src []byte, filename string) (*hcl.File, hcl.Diagnostics) {
	tokens, _ := hclsyntax.LexConfig(src, filename, hcl.InitialPos) // the parser reports what does not scan
	if diags := check(tokens); diags.HasErrors() {
		return nil, diags
	}

	return hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
}

// ParseExpression is hclsyntax.ParseExpression within the bounds.
func ParseExpression(src []byte, filename string) (hclsyntax.Expression, hcl.Diagnostics) {
	tokens, _ := hclsyntax.LexExpression(src, filename, hcl.InitialPos) // the parser reports what does not scan
	if diags := check(tokens); diags.HasErrors() {
		return nil, diags
	}

	return hclsyntax.ParseExpression(src, filename, hcl.InitialPos)
}

// ParseTemplate is hclsyntax.ParseTemplate within the bounds.
func ParseTemplate(src []byte, filename string) (hclsyntax.Expression, hcl.Diagnostics) {
	tokens, _ := hclsyntax.LexTemplate(src, filename, hcl.InitialPos) // the parser reports what does not scan
	if diags := check(tokens); diags.HasErrors() {
		return nil, diags
	}

	return hclsyntax.ParseTemplate(src, filename, hcl.InitialPos)
}

// closerOf returns the token that closes what t opens.
func closerOf(t hclsyntax.TokenType) (hclsyntax.TokenType, bool) {
	switch t { //nolint:exhaustive // only the tokens that open
	case hclsyntax.TokenOParen:
		return hclsyntax.TokenCParen, true
	case hclsyntax.TokenOBrack:
		return hclsyntax.TokenCBrack, true
	case hclsyntax.TokenOBrace:
		return hclsyntax.TokenCBrace, true
	case hclsyntax.TokenOQuote:
		return hclsyntax.TokenCQuote, true
	case hclsyntax.TokenOHeredoc:
		return hclsyntax.TokenCHeredoc, true
	case hclsyntax.TokenTemplateInterp, hclsyntax.TokenTemplateControl:
		return hclsyntax.TokenTemplateSeqEnd, true
	default:
		return hclsyntax.TokenNil, false
	}
}

// deepens reports a token that counts against MaxOperators.
func deepens(t hclsyntax.TokenType) bool {
	switch t { //nolint:exhaustive // only the tokens that deepen
	case hclsyntax.TokenQuestion, hclsyntax.TokenBang, hclsyntax.TokenMinus, hclsyntax.TokenPlus,
		hclsyntax.TokenStar, hclsyntax.TokenSlash, hclsyntax.TokenPercent,
		hclsyntax.TokenEqualOp, hclsyntax.TokenNotEqual, hclsyntax.TokenAnd, hclsyntax.TokenOr,
		hclsyntax.TokenGreaterThan, hclsyntax.TokenGreaterThanEq, hclsyntax.TokenLessThan, hclsyntax.TokenLessThanEq,
		hclsyntax.TokenTemplateControl, hclsyntax.TokenCBrack, hclsyntax.TokenCBrace:
		return true
	default:
		return false
	}
}

// check reports the first token over a bound. A closer that does not match
// closes nothing: the parser may still be inside what is open.
func check(tokens hclsyntax.Tokens) hcl.Diagnostics {
	open := make([]hclsyntax.TokenType, 0, MaxDepth)

	var operators int

	for _, t := range tokens {
		if deepens(t.Type) {
			operators++
			if operators > MaxOperators {
				return tooComplex(errOperators, t.Range)
			}
		}

		if closer, ok := closerOf(t.Type); ok {
			if len(open) == MaxDepth {
				return tooComplex(errDepth, t.Range)
			}

			open = append(open, closer)
		} else if len(open) > 0 && open[len(open)-1] == t.Type {
			open = open[:len(open)-1]
		}
	}

	return nil
}

func tooComplex(err error, rng hcl.Range) hcl.Diagnostics {
	return hcl.Diagnostics{{
		Severity: hcl.DiagError, Summary: Summary,
		Detail: "The text can't be read: " + err.Error() + ".", Subject: rng.Ptr(),
	}}
}

// ErrJSON is the error of JSON over the bound.
var ErrJSON = errors.New("the JSON can't be read")

// CheckJSON refuses JSON nested deeper than MaxDepth: the decoder of cty
// recurses with it, without a bound of its own.
func CheckJSON(src []byte) error {
	var (
		depth             int
		inString, escaped bool
	)

	for _, c := range src {
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '[' || c == '{':
			depth++
			if depth > MaxDepth {
				return fmt.Errorf("%w: %w", ErrJSON, errDepth)
			}
		case (c == ']' || c == '}') && depth > 0:
			depth--
		}
	}

	return nil
}
