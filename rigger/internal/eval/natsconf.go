// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/bacanoo-io/norca-rig/runref"
)

// natsconf is natsconf(config): the configuration file of a NATS server
// written from an object (design 4.3). A null attribute is left out. A
// string is double-quoted and escaped; a runtime reference in it (package
// runref) is quoted for where it sits: a whole value quoted, inside a
// string escaped. An item of a list from service_addresses() is written as
// the addresses the task template finds, one per line.
var natsconf = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "config", Type: cty.DynamicPseudoType}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		w := &confWriter{}
		if err := w.top(args[0]); err != nil {
			return cty.NilVal, err
		}

		return cty.StringVal(w.b.String()), nil
	},
})

// confWriter writes a NATS configuration.
type confWriter struct {
	b strings.Builder
}

// confKey is a key NATS reads without quotes.
var confKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// errNotKnown is the error of a value that is not known at install time.
var errNotKnown = errors.New("the value is not known at install time")

func (w *confWriter) top(v cty.Value) error {
	if !v.IsWhollyKnown() {
		return errNotKnown
	}

	if v.IsNull() || (!v.Type().IsObjectType() && !v.Type().IsMapType()) {
		return errors.New("the configuration is an object")
	}

	return w.entries(v, 0)
}

// entries writes the attributes of an object, sorted by name.
func (w *confWriter) entries(v cty.Value, depth int) error {
	m := v.AsValueMap()
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	for _, k := range keys {
		if err := w.entry(k, m[k], depth); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}

	return nil
}

func (w *confWriter) indent(depth int) {
	w.b.WriteString(strings.Repeat("  ", depth))
}

func (w *confWriter) entry(k string, v cty.Value, depth int) error {
	if v.IsNull() {
		return nil
	}

	key := k
	if !confKey.MatchString(k) {
		key = `"` + escapeConf(k) + `"`
	}

	ty := v.Type()

	w.indent(depth)

	switch {
	case ty.IsObjectType() || ty.IsMapType():
		w.b.WriteString(key + " {\n")

		if err := w.entries(v, depth+1); err != nil {
			return err
		}

		w.indent(depth)
		w.b.WriteString("}\n")
	case ty.IsListType() || ty.IsTupleType() || ty.IsSetType():
		w.b.WriteString(key + ": [\n")

		if err := w.items(v, depth+1); err != nil {
			return err
		}

		w.indent(depth)
		w.b.WriteString("]\n")
	default:
		s, err := scalarConf(v)
		if err != nil {
			return err
		}

		w.b.WriteString(key + ": " + s + "\n")
	}

	return nil
}

// items writes the items of a list, one per line.
func (w *confWriter) items(v cty.Value, depth int) error {
	i := 0

	for it := v.ElementIterator(); it.Next(); i++ {
		_, item := it.Element()
		if item.IsNull() {
			continue
		}

		if err := w.item(item, depth); err != nil {
			return fmt.Errorf("[%d]: %w", i, err)
		}
	}

	return nil
}

func (w *confWriter) item(v cty.Value, depth int) error {
	ty := v.Type()

	switch {
	case ty.IsObjectType() || ty.IsMapType():
		w.indent(depth)
		w.b.WriteString("{\n")

		if err := w.entries(v, depth+1); err != nil {
			return err
		}

		w.indent(depth)
		w.b.WriteString("}\n")

		return nil
	case ty == cty.String:
		if p, ok := wholePart(v.AsString()); ok && p.Kind == runref.KindServices {
			if p.Quote == runref.QuoteAuto {
				p.Quote = runref.QuoteConf
			}

			// The template writes the lines; it is not indented.
			w.b.WriteString(p.String() + "\n")

			return nil
		}
	}

	s, err := scalarConf(v)
	if err != nil {
		return err
	}

	w.indent(depth)
	w.b.WriteString(s + "\n")

	return nil
}

// scalarConf writes a string, a number or a bool.
func scalarConf(v cty.Value) (string, error) {
	switch v.Type() {
	case cty.String:
		return stringConf(v.AsString())
	case cty.Number:
		f := v.AsBigFloat()
		if f.IsInt() {
			i, _ := f.Int(nil)

			return i.String(), nil
		}

		return f.Text('f', -1), nil
	case cty.Bool:
		if v.True() {
			return "true", nil
		}

		return "false", nil
	default:
		return "", fmt.Errorf("a value of type %s can't be written", v.Type().FriendlyName())
	}
}

// stringConf writes a string, double-quoted. A runtime reference that is the
// whole string is quoted when the template renders; one inside a string is
// escaped there.
func stringConf(s string) (string, error) {
	if p, ok := wholePart(s); ok {
		if p.Kind == runref.KindServices {
			return "", errors.New("service_addresses() gives a list; use it as one")
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
			b.WriteString(escapeConf(seg.Text))

			continue
		}

		p := *seg.Part
		if p.Kind == runref.KindServices {
			return "", errors.New("service_addresses() gives a list; use it as one")
		}

		if p.Quote == runref.QuoteAuto {
			p.Quote = runref.QuoteConfEscape
		}

		b.WriteString(p.String())
	}

	b.WriteByte('"')

	return b.String(), nil
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

// escapeConf escapes a string of a NATS configuration.
var escapeConf = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`).Replace
