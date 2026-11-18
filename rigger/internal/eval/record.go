// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/bacanoo-io/norca-rig/internal/lang"
)

// RecordedValues returns the values of an object a revision records: every
// attribute but those that hold a sensitive value (design 6.3).
func RecordedValues(v cty.Value) (map[string]any, error) {
	out := map[string]any{}

	if v.IsNull() || !isObject(v) {
		return out, nil
	}

	for name, item := range v.AsValueMap() {
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
	v, err := cty.Transform(v, func(_ cty.Path, v cty.Value) (cty.Value, error) {
		if !v.IsMarked() {
			return v, nil
		}

		inner, _ := v.UnmarkDeep()

		data, err := ctyjson.Marshal(inner, inner.Type())
		if err != nil {
			return cty.NilVal, err
		}

		sum := sha256.Sum256(data)

		return cty.StringVal("sensitive:" + hex.EncodeToString(sum[:])), nil
	})
	if err != nil {
		return nil, err
	}

	v, _ = v.UnmarkDeep()

	return ctyjson.Marshal(v, v.Type())
}

// MarkSensitive marks the attributes of values whose variable in m is
// sensitive, so that a revision does not record them.
func MarkSensitive(m *lang.Module, values cty.Value) cty.Value {
	if values.IsNull() || !isObject(values) {
		return values
	}

	attrs := values.AsValueMap()
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
