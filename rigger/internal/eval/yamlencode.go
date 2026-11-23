// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// yamlEncode is yamlencode(value): the value as a YAML document in block
// style. Every string is double-quoted, so that no value reads as another
// type; a list that ends with the addresses of a service is written inline.
var yamlEncode = encoder(func(v cty.Value) (string, error) {
	w := &yamlWriter{}
	if err := w.document(v); err != nil {
		return "", err
	}

	return w.b.String(), nil
})

// yamlWriter writes YAML.
type yamlWriter struct {
	b strings.Builder
}

// yamlKey is a key YAML reads as a string without quotes.
var yamlKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// yamlReserved are plain words YAML reads as something else than a string.
var yamlReserved = map[string]bool{
	"true": true, "false": true, "yes": true, "no": true, "on": true, "off": true,
	"y": true, "n": true, nullText: true, "~": true,
}

func yamlKeyText(k string) string {
	if yamlKey.MatchString(k) && !yamlReserved[strings.ToLower(k)] {
		return k
	}

	return `"` + escapeString(k) + `"`
}

func (w *yamlWriter) document(v cty.Value) error {
	switch {
	case !v.IsNull() && isObject(v) && v.LengthInt() > 0:
		return w.entries(v, 0)
	case !v.IsNull() && isList(v) && v.LengthInt() > 0:
		return w.items(v, 0)
	default:
		s, err := w.inline(v)
		if err != nil {
			return err
		}

		w.b.WriteString(s + "\n")

		return nil
	}
}

func (w *yamlWriter) indent(depth int) string {
	return strings.Repeat("  ", depth)
}

// entries writes the attributes of an object, one per line.
func (w *yamlWriter) entries(v cty.Value, depth int) error {
	keys, m := sortedKeys(v)

	for _, k := range keys {
		w.b.WriteString(w.indent(depth) + yamlKeyText(k) + ":")

		if err := w.nested(m[k], depth+1); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}

	return nil
}

// nested writes the value of an attribute or of an item, after its key or
// its dash: on the same line, or on the lines below, indented to depth.
func (w *yamlWriter) nested(v cty.Value, depth int) error {
	switch {
	case !v.IsNull() && isObject(v) && v.LengthInt() > 0:
		w.b.WriteString("\n")

		return w.entries(v, depth)
	case !v.IsNull() && isList(v) && v.LengthInt() > 0 && !endsWithAddresses(v):
		w.b.WriteString("\n")

		return w.items(v, depth)
	default:
		s, err := w.inline(v)
		if err != nil {
			return err
		}

		w.b.WriteString(" " + s + "\n")

		return nil
	}
}

// items writes the items of a list, one per line after a dash.
func (w *yamlWriter) items(v cty.Value, depth int) error {
	i := 0

	for it := v.ElementIterator(); it.Next(); i++ {
		_, item := it.Element()

		if err := w.item(item, depth); err != nil {
			return fmt.Errorf("[%d]: %w", i, err)
		}
	}

	return nil
}

// item writes an item of a list. An object or a list goes in block style,
// its first line after the dash: "- name: x".
func (w *yamlWriter) item(v cty.Value, depth int) error {
	sub := &yamlWriter{}

	switch {
	case !v.IsNull() && isObject(v) && v.LengthInt() > 0:
		if err := sub.entries(v, depth+1); err != nil {
			return err
		}
	case !v.IsNull() && isList(v) && v.LengthInt() > 0 && !endsWithAddresses(v):
		if err := sub.items(v, depth+1); err != nil {
			return err
		}
	default:
		w.b.WriteString(w.indent(depth) + "-")

		return w.nested(v, depth+1)
	}

	w.b.WriteString(w.indent(depth) + "- " + strings.TrimPrefix(sub.b.String(), w.indent(depth+1)))

	return nil
}

// inline writes a value on one line: a scalar, null, an empty collection,
// or a list that ends with the addresses of a service, with all that is in
// it: a list in brackets, an object as a mapping in braces.
func (w *yamlWriter) inline(v cty.Value) (string, error) {
	switch {
	case v.IsNull():
		return nullText, nil
	case isObject(v):
		keys, m := sortedKeys(v)
		parts := make([]string, 0, len(keys))

		for _, k := range keys {
			s, err := w.inline(m[k])
			if err != nil {
				return "", fmt.Errorf("%s: %w", k, err)
			}

			parts = append(parts, yamlKeyText(k)+": "+s)
		}

		return "{" + strings.Join(parts, ", ") + "}", nil
	case isList(v):
		items, services, err := listItems(v, true)
		if err != nil {
			return "", err
		}

		return inlineList(items, services, ", ", w.inline)
	default:
		return scalarText(v)
	}
}

// endsWithAddresses reports whether a list ends with the addresses of a
// service, so that it is written inline.
func endsWithAddresses(v cty.Value) bool {
	_, services, err := listItems(v, true)

	return err != nil || services != nil
}
