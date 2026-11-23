// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/bacanoo-io/norca/rigger/internal/syntax"
	"github.com/bacanoo-io/norca/rigger/runref"
)

// The encoders write a configuration file from data (design 4.3): natsconf,
// jsonencode, yamlencode, tomlencode. Their output is the data of a task
// template, so a runtime value in it (package runref) is quoted for where it
// sits: a whole string by the template, a part of a string escaped inside
// the quotes the encoder writes. The addresses of service_addresses() are
// written as the last items of an inline list.

// encoder returns a function of one value that writes it with write.
func encoder(write func(cty.Value) (string, error)) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: attrValue, Type: cty.DynamicPseudoType, AllowNull: true}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if !args[0].IsWhollyKnown() {
				return cty.NilVal, errNotKnown
			}

			s, err := write(args[0])
			if err != nil {
				return cty.NilVal, err
			}

			return cty.StringVal(s), nil
		},
	})
}

// summaryInvalidValue is the summary of a value that does not fit.
const summaryInvalidValue = "Invalid value"

// nullText is null in JSON, YAML and the messages of a validation.
const nullText = "null"

// errNotKnown is the error of a value that is not known at install time.
var errNotKnown = errors.New("the value is not known at install time")

// errAddressesNotList is the error of service_addresses() used as a string.
var errAddressesNotList = errors.New("service_addresses() gives a list; use it as one")

// format is what the formats of the encoders do not write the same way: the
// escape of a character by its code, and a whole number of more than 64
// bits.
type format struct {
	// hexEscapes writes a control character as \xHH, the only escape of a
	// character by its code that NATS reads. The other formats read \uXXXX.
	hexEscapes bool
	// int64Only says that the format reads a whole number into 64 bits and
	// refuses a larger one, which is written as a float then.
	int64Only bool
}

// The formats of the four encoders.
var (
	jsonFormat = format{}
	yamlFormat = format{}
	tomlFormat = format{int64Only: true}
	confFormat = format{hexEscapes: true, int64Only: true}
)

// quote writes s as a double-quoted string, with \, ", the line breaks and
// the tab escaped, which the four formats read the same way, and the other
// characters the format does not take as they are escaped by their code.
func (f format) quote(s string) (string, error) {
	if p, ok := wholePart(s); ok {
		if p.Kind == runref.KindServices {
			return "", errAddressesNotList
		}

		if p.Quote == runref.QuoteAuto {
			p.Quote = runref.QuoteConf
		}

		return p.String(), nil
	}

	segs, err := runref.Split(s)
	if err != nil {
		return "", err
	}

	var b strings.Builder

	b.WriteByte('"')

	for _, seg := range segs {
		if seg.Part == nil {
			b.WriteString(f.escape(seg.Text))

			continue
		}

		p := *seg.Part
		if p.Kind == runref.KindServices {
			return "", errAddressesNotList
		}

		if p.Quote == runref.QuoteAuto {
			p.Quote = runref.QuoteConfEscape
		}

		b.WriteString(p.String())
	}

	b.WriteByte('"')

	return b.String(), nil
}

// escape escapes a string for the inside of double quotes.
func (f format) escape(s string) string {
	var b strings.Builder

	for _, r := range s {
		switch {
		case r == '\\' || r == '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case f.hexEscapes && isControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case !f.hexEscapes && needsCode(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
}

// isControl reports a control character of one byte: \xHH of NATS is a
// byte. NATS has no escape for a longer character, and reads every
// character in a string as it is.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// needsCode reports a character that JSON, YAML or TOML does not take as it
// is in a quoted string, or reads as another one: the control characters
// (a reader of YAML refuses them, and reads NEL, U+0085, as a line break),
// the line and the paragraph separator (line breaks too for YAML 1.1), the
// byte order mark, and U+FFFE and U+FFFF, which are no characters. All of
// them fit \uXXXX.
func needsCode(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r == 0x2028, r == 0x2029, r == 0xfeff, r == 0xfffe, r == 0xffff:
		return true
	default:
		return false
	}
}

// wholePart returns the part that is all of s.
func wholePart(s string) (runref.Part, bool) {
	if !runref.Contains(s) {
		return runref.Part{}, false
	}

	segs, err := runref.Split(s)
	if err != nil || len(segs) != 1 || segs[0].Part == nil {
		return runref.Part{}, false
	}

	return *segs[0].Part, true
}

// number writes a number in digits, a whole one without a point, and none
// with an exponent: not every reader takes one. NATS reads 1e-07 as a
// string and refuses 1.5e-05, and a reader of YAML 1.1 takes a number with
// an exponent and no point for a string.
func (f format) number(v cty.Value) (string, error) {
	n := v.AsBigFloat()

	text, err := numberText(n)
	if err != nil {
		return "", err
	}

	if n.Sign() == 0 {
		return "0", nil
	}

	if f.int64Only && n.IsInt() {
		if i, _ := n.Int(nil); !i.IsInt64() {
			return text + ".0", nil
		}
	}

	return text, nil
}

// errInfinite is the error of an infinite number, what 1/0 and log(0, 10)
// give: it has no digits, and no format has another way to write it.
var errInfinite = errors.New("the number is infinite and can't be written")

// errDigits is the error of a number too long to write: the time to write
// one grows by the square of its digits, where no time limit is looked at.
// A literal is bounded as the module is read (syntax.MaxNumberDigits); this
// is the same bound for a number that was computed, or read from JSON.
var errDigits = fmt.Errorf("the number has more than %d digits, the limit of the engine", syntax.MaxNumberDigits)

// The binary exponents past which a number has more digits than the bound
// whatever its digits are: before the point from 2^3322, which is over
// 10^1000, and after it under 2^-3326, which is under 10^-1001 (a digit of
// ten is 3.3219 bits). Between them the text of the number is short.
const (
	maxWholeExp    = syntax.MaxNumberDigits * 3322 / 1000
	minFractionExp = -(syntax.MaxNumberDigits+1)*3322/1000 - 1
)

// numberText writes a number in digits, without an exponent, or says why
// it can't be written. The exponent is read first, which costs nothing: a
// number far over the bound is refused before any of it is written. The
// digits are counted as package syntax counts those of a literal, so that a
// number written into a text the engine reads again is read there.
func numberText(n *big.Float) (string, error) {
	if n.IsInf() {
		return "", errInfinite
	}

	if exp := n.MantExp(nil); exp > maxWholeExp || exp < minFractionExp {
		return "", errDigits
	}

	text := n.Text('f', -1)

	whole, fraction, _ := strings.Cut(strings.TrimPrefix(text, "-"), ".")
	if len(strings.TrimLeft(whole, "0"))+len(fraction) > syntax.MaxNumberDigits {
		return "", errDigits
	}

	return text, nil
}

// numberError is the error of a number that can't be written.
func numberError(n *big.Float) error {
	_, err := numberText(n)

	return err
}

// digitsError is the error of a number of more digits than the bound. An
// infinite number has none: it is an error only where it is written.
func digitsError(n *big.Float) error {
	if n.IsInf() {
		return nil
	}

	return numberError(n)
}

// checkNumbers reports the first number in v that can't be written.
func checkNumbers(v cty.Value) error {
	return walkNumbers(v, numberError)
}

// walkNumbers reports the first number in v that check refuses.
func walkNumbers(v cty.Value, check func(*big.Float) error) error {
	return cty.Walk(v, func(_ cty.Path, v cty.Value) (bool, error) {
		v, _ = v.Unmark()
		if v.IsNull() || !v.IsKnown() || v.Type() != cty.Number {
			return true, nil
		}

		return true, check(v.AsBigFloat())
	})
}

// scalar writes a string, a number or a bool.
func (f format) scalar(v cty.Value) (string, error) {
	switch v.Type() {
	case cty.String:
		return f.quote(v.AsString())
	case cty.Number:
		return f.number(v)
	case cty.Bool:
		return strconv.FormatBool(v.True()), nil
	default:
		return "", fmt.Errorf("a value of type %s is not a string, a number or a bool here", v.Type().FriendlyName())
	}
}

// isObject reports whether v is an object or a map.
func isObject(v cty.Value) bool {
	return v.Type().IsObjectType() || v.Type().IsMapType()
}

// isList reports whether v is a list, a set or a tuple.
func isList(v cty.Value) bool {
	ty := v.Type()

	return ty.IsListType() || ty.IsSetType() || ty.IsTupleType()
}

// sortedKeys returns the attributes of an object by name.
func sortedKeys(v cty.Value) ([]string, map[string]cty.Value) {
	m := v.AsValueMap()
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys, m
}

// listItems returns the items of a list, and the addresses of
// service_addresses() when the list ends with them. With keepNull a null
// item is kept, else left out.
func listItems(v cty.Value, keepNull bool) ([]cty.Value, *runref.Part, error) {
	var (
		items    []cty.Value
		services *runref.Part
	)

	for it := v.ElementIterator(); it.Next(); {
		_, item := it.Element()

		switch {
		case item.IsNull() && !keepNull:
			continue
		case services != nil:
			return nil, nil, errors.New("the addresses of service_addresses() go at the end of a list")
		case !item.IsNull() && item.Type() == cty.String:
			if p, ok := wholePart(item.AsString()); ok && p.Kind == runref.KindServices {
				services = &p

				continue
			}
		}

		items = append(items, item)
	}

	return items, services, nil
}

// inlineList writes a list on one line: its items written by item and
// separated by sep, and then the addresses of a service.
func inlineList(items []cty.Value, services *runref.Part, sep string, item func(cty.Value) (string, error)) (string, error) {
	parts := make([]string, 0, len(items))

	for i, v := range items {
		s, err := item(v)
		if err != nil {
			return "", fmt.Errorf("[%d]: %w", i, err)
		}

		parts = append(parts, s)
	}

	out := "[" + strings.Join(parts, sep)

	if services != nil {
		p := *services
		if p.Quote == runref.QuoteAuto {
			p.Quote = runref.QuoteConf
		}

		p.Sep = sep
		if len(items) > 0 {
			p.Lead = sep
		}

		out += p.String()
	}

	return out + "]", nil
}
