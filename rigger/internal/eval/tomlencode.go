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

// tomlEncode is tomlencode(value): an object as a TOML document. The
// values of a table come before its subtables; an object is a table, a
// list of objects an array of tables. TOML has no null: a null attribute is
// left out.
var tomlEncode = encoder(func(v cty.Value) (string, error) {
	if v.IsNull() || !isObject(v) {
		return "", errors.New("a TOML document is an object")
	}

	w := &tomlWriter{}
	if err := w.table(nil, v); err != nil {
		return "", err
	}

	return strings.TrimPrefix(w.b.String(), "\n"), nil
})

// tomlWriter writes TOML.
type tomlWriter struct {
	b strings.Builder
}

// tomlKey is a bare key of TOML.
var tomlKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKeyText(k string) string {
	if tomlKey.MatchString(k) {
		return k
	}

	return `"` + tomlFormat.escape(k) + `"`
}

func tomlPath(path []string) string {
	keys := make([]string, len(path))
	for i, k := range path {
		keys[i] = tomlKeyText(k)
	}

	return strings.Join(keys, ".")
}

// table writes the values of a table, then its subtables and arrays of
// tables.
func (w *tomlWriter) table(path []string, v cty.Value) error {
	keys, m := sortedKeys(v)

	var tables, arrays []string

	for _, k := range keys {
		item := m[k]

		switch {
		case item.IsNull():
		case isObject(item):
			tables = append(tables, k)
		case isList(item) && isTableArray(item):
			arrays = append(arrays, k)
		default:
			s, err := tomlInline(item)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}

			w.b.WriteString(tomlKeyText(k) + " = " + s + "\n")
		}
	}

	for _, k := range tables {
		sub := append(append([]string{}, path...), k)
		w.b.WriteString("\n[" + tomlPath(sub) + "]\n")

		if err := w.table(sub, m[k]); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}

	for _, k := range arrays {
		sub := append(append([]string{}, path...), k)

		for it := m[k].ElementIterator(); it.Next(); {
			_, item := it.Element()

			w.b.WriteString("\n[[" + tomlPath(sub) + "]]\n")

			if err := w.table(sub, item); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	}

	return nil
}

// isTableArray reports whether a list is a non-empty list of objects only.
func isTableArray(v cty.Value) bool {
	if v.LengthInt() == 0 {
		return false
	}

	for it := v.ElementIterator(); it.Next(); {
		_, item := it.Element()
		if item.IsNull() || !isObject(item) {
			return false
		}
	}

	return true
}

// tomlInline writes a value on one line: a scalar, or an inline array.
func tomlInline(v cty.Value) (string, error) {
	switch {
	case v.IsNull():
		return "", errors.New("TOML has no null")
	case isObject(v):
		return "", errors.New("an object is a table, not an item of a list")
	case isList(v):
		items, services, err := listItems(v, true)
		if err != nil {
			return "", err
		}

		return inlineList(items, services, ", ", tomlInline)
	default:
		return tomlFormat.scalar(v)
	}
}
