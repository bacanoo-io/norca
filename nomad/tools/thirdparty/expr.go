// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"strings"
)

// exprAllowed reports whether an SPDX license expression is satisfied by the
// allowed licenses: every operand of AND must be allowed, and at least one
// operand of OR. An unparsable expression is not allowed.
func exprAllowed(expr string, allowed map[string]bool) bool {
	p := &exprParser{toks: tokenize(expr), allowed: allowed}
	ok, err := p.parseOr()
	if err != nil || p.pos != len(p.toks) {
		return false
	}
	return ok
}

func tokenize(s string) []string {
	s = strings.ReplaceAll(s, "(", " ( ")
	s = strings.ReplaceAll(s, ")", " ) ")
	return strings.Fields(s)
}

type exprParser struct {
	toks    []string
	pos     int
	allowed map[string]bool
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *exprParser) parseOr() (bool, error) {
	ok, err := p.parseAnd()
	if err != nil {
		return false, err
	}
	for strings.EqualFold(p.peek(), "OR") {
		p.pos++
		r, err := p.parseAnd()
		if err != nil {
			return false, err
		}
		ok = ok || r
	}
	return ok, nil
}

func (p *exprParser) parseAnd() (bool, error) {
	ok, err := p.parseAtom()
	if err != nil {
		return false, err
	}
	for strings.EqualFold(p.peek(), "AND") {
		p.pos++
		r, err := p.parseAtom()
		if err != nil {
			return false, err
		}
		ok = ok && r
	}
	return ok, nil
}

func (p *exprParser) parseAtom() (bool, error) {
	t := p.peek()
	switch {
	case t == "":
		return false, fmt.Errorf("unexpected end of expression")
	case t == "(":
		p.pos++
		ok, err := p.parseOr()
		if err != nil {
			return false, err
		}
		if p.peek() != ")" {
			return false, fmt.Errorf("missing )")
		}
		p.pos++
		return ok, nil
	case t == ")" || strings.EqualFold(t, "AND") || strings.EqualFold(t, "OR") || strings.EqualFold(t, "WITH"):
		return false, fmt.Errorf("unexpected %q", t)
	}
	p.pos++
	ok := p.allowed[t]
	if strings.EqualFold(p.peek(), "WITH") {
		// License exceptions are not on the allow-list.
		p.pos += 2
		ok = false
	}
	return ok, nil
}
