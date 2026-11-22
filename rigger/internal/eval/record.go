// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// RecordedValues returns the values of an object a revision records: every
// attribute but those that hold a sensitive value (design 6.3).
func RecordedValues(v cty.Value) (map[string]any, error) {
	out := map[string]any{}

	if v.IsNull() || !isObject(v) {
		return out, nil
	}

	for name, item := range attributeValues(v) {
		if item.ContainsMarked() || !item.IsWhollyKnown() {
			continue
		}

		data, err := ctyjson.Marshal(item, item.Type())
		if err != nil {
			return nil, err
		}

		var plain any
		if err := json.Unmarshal(data, &plain); err != nil {
			return nil, err
		}

		out[name] = plain
	}

	return out, nil
}

// DigestValues returns the values as JSON for the digest of a release: a
// sensitive value stands as the SHA-256 of its JSON, so that a change of a
// password changes the digest without the digest showing it.
func DigestValues(v cty.Value) (json.RawMessage, error) {
	v, err := redact(v)
	if err != nil {
		return nil, err
	}

	return ctyjson.Marshal(v, v.Type())
}

// redact returns v with each sensitive value as its hash. A collection
// becomes a tuple or an object, which write the same JSON: a list of
// numbers with one hash in it is no list of one type.
func redact(v cty.Value) (cty.Value, error) {
	out, err := redactParts(v)
	if err != nil || !v.IsMarked() {
		return out, err
	}

	data, err := ctyjson.Marshal(out, out.Type())
	if err != nil {
		return cty.NilVal, err
	}

	sum := sha256.Sum256(data)

	return cty.StringVal("sensitive:" + hex.EncodeToString(sum[:])), nil
}

// redactParts redacts the parts of v, without the marks of v. The
// attributes of a sensitive object are sensitive, each hashed before the
// object is; the items of a collection have only marks of their own. The
// recorded digests depend on it.
func redactParts(v cty.Value) (cty.Value, error) {
	inner, _ := v.Unmark()
	if inner.IsNull() || !inner.IsKnown() {
		return inner, nil
	}

	switch {
	case isList(inner):
		var items []cty.Value

		for it := inner.ElementIterator(); it.Next(); {
			_, item := it.Element()

			out, err := redact(item)
			if err != nil {
				return cty.NilVal, err
			}

			items = append(items, out)
		}

		return cty.TupleVal(items), nil
	case isObject(inner):
		parts := inner.AsValueMap()
		if inner.Type().IsObjectType() {
			parts = attributeValues(v)
		}

		attrs := make(map[string]cty.Value, len(parts))

		for name, item := range parts {
			out, err := redact(item)
			if err != nil {
				return cty.NilVal, err
			}

			attrs[name] = out
		}

		return cty.ObjectVal(attrs), nil
	default:
		return inner, nil
	}
}

// MarkSensitive marks the attributes of values whose variable in m is
// sensitive, so that a revision does not record them.
func MarkSensitive(m *lang.Module, values cty.Value) cty.Value {
	if values.IsNull() || !isObject(values) {
		return values
	}

	attrs := attributeValues(values)
	for name, v := range attrs {
		if decl, ok := m.Variable(name); ok && decl.Sensitive {
			attrs[name] = v.Mark(Sensitive)
		}
	}

	if len(attrs) == 0 {
		return cty.EmptyObjectVal
	}

	return cty.ObjectVal(attrs)
}
