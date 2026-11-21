// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

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

// quoteString writes s as a double-quoted string, with \, ", and the
// control characters escaped, which the four formats read the same way.
func quoteString(s string) (string, error) {
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
			b.WriteString(escapeString(seg.Text))

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

// escapeString escapes a string for the inside of double quotes.
func escapeString(s string) string {
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
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
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

// numberText writes a number: an integer without a point.
func numberText(v cty.Value) string {
	f := v.AsBigFloat()
	if f.IsInt() {
		i, _ := f.Int(nil)

		return i.String()
	}

	return f.Text('g', -1)
}

// scalarText writes a string, a number or a bool.
func scalarText(v cty.Value) (string, error) {
	switch v.Type() {
	case cty.String:
		return quoteString(v.AsString())
	case cty.Number:
		return numberText(v), nil
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
