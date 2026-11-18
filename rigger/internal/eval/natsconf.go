// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// natsconf is natsconf(config): the configuration file of a NATS server
// written from an object. A null attribute is left out.
var natsconf = encoder(func(v cty.Value) (string, error) {
	if v.IsNull() || !isObject(v) {
		return "", errors.New("the configuration is an object")
	}

	w := &confWriter{}
	if err := w.entries(v, 0); err != nil {
		return "", err
	}

	return w.b.String(), nil
})

// confWriter writes a NATS configuration.
type confWriter struct {
	b strings.Builder
}

// confKey is a key NATS reads without quotes.
var confKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func (w *confWriter) entries(v cty.Value, depth int) error {
	keys, m := sortedKeys(v)

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
		key = `"` + escapeString(k) + `"`
	}

	switch {
	case isObject(v):
		w.indent(depth)
		w.b.WriteString(key + " {\n")

		if err := w.entries(v, depth+1); err != nil {
			return err
		}

		w.indent(depth)
		w.b.WriteString("}\n")

		return nil
	case isList(v):
		return w.list(key, v, depth)
	default:
		s, err := scalarText(v)
		if err != nil {
			return err
		}

		w.indent(depth)
		w.b.WriteString(key + ": " + s + "\n")

		return nil
	}
}

// list writes a list one item per line, or on one line when it ends with
// the addresses of a service.
func (w *confWriter) list(key string, v cty.Value, depth int) error {
	items, services, err := listItems(v, false)
	if err != nil {
		return err
	}

	w.indent(depth)

	if services != nil {
		s, err := inlineList(items, services, ", ", scalarText)
		if err != nil {
			return err
		}

		w.b.WriteString(key + ": " + s + "\n")

		return nil
	}

	w.b.WriteString(key + ": [\n")

	for i, item := range items {
		if err := w.item(item, depth+1); err != nil {
			return fmt.Errorf("[%d]: %w", i, err)
		}
	}

	w.indent(depth)
	w.b.WriteString("]\n")

	return nil
}

func (w *confWriter) item(v cty.Value, depth int) error {
	w.indent(depth)

	if isObject(v) {
		w.b.WriteString("{\n")

		if err := w.entries(v, depth+1); err != nil {
			return err
		}

		w.indent(depth)
		w.b.WriteString("}\n")

		return nil
	}

	s, err := scalarText(v)
	if err != nil {
		return err
	}

	w.b.WriteString(s + "\n")

	return nil
}
