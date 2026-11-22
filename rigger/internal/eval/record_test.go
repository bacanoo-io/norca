// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// hashedTwo is how the digest holds the sensitive number 2.
func hashedTwo() string {
	sum := sha256.Sum256([]byte("2"))

	return `"sensitive:` + hex.EncodeToString(sum[:]) + `"`
}

func TestDigestValues(t *testing.T) {
	t.Parallel()

	one, two := cty.NumberIntVal(1), cty.NumberIntVal(2)

	tests := []struct {
		name     string
		value    cty.Value
		expected string
	}{
		{name: "list", value: cty.ListVal([]cty.Value{one, two}), expected: `[1,2]`},
		{name: "empty list", value: cty.ListValEmpty(cty.Number), expected: `[]`},
		{name: "set", value: cty.SetVal([]cty.Value{two, one}), expected: `[1,2]`},
		{name: "map", value: cty.MapVal(map[string]cty.Value{"b": two, "a": one}), expected: `{"a":1,"b":2}`},
		{name: "empty map", value: cty.MapValEmpty(cty.Number), expected: `{}`},
		{name: "null", value: cty.NullVal(cty.String), expected: `null`},
		{name: "sensitive", value: two.Mark(Sensitive), expected: hashedTwo()},
		// The digests that releases already record: the attributes of a
		// sensitive object are hashed one by one and then the object, the
		// elements of a sensitive map only together.
		{
			name:     "sensitive object",
			value:    cty.ObjectVal(map[string]cty.Value{"k0": cty.True}).Mark(Sensitive),
			expected: `"sensitive:40dc5351c87c0d3276d948782d51a9e6b7d7a1eeae8f914ebec700bc5017b1a0"`,
		},
		{
			name:     "sensitive empty map",
			value:    cty.MapValEmpty(cty.String).Mark(Sensitive),
			expected: `"sensitive:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"`,
		},
		{name: "sensitive item of a list", value: cty.ListVal([]cty.Value{one, two.Mark(Sensitive)}), expected: `[1,` + hashedTwo() + `]`},
		{
			name:     "sensitive element of a map",
			value:    cty.MapVal(map[string]cty.Value{"a": one, "b": two.Mark(Sensitive)}),
			expected: `{"a":1,"b":` + hashedTwo() + `}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()

			got, err := DigestValues(cty.ObjectVal(map[string]cty.Value{"v": tt.value}))
			if err != nil {
				t.Fatal(err)
			}

			if want := `{"v":` + tt.expected + `}`; string(got) != want {
				t.Errorf("DigestValues() = %s, want %s", got, want)
			}
		})
	}
}
