// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// log and pow compute in float64, which has a value for what is not a
// number, the logarithm of a negative number or its square root. cty has
// none: such a call is an error that says so.
func TestFunctions_NotANumber(t *testing.T) {
	t.Parallel()

	num := cty.MustParseNumberVal
	log, pow := Functions()["log"], Functions()["pow"]

	tests := []struct {
		name     string
		fn       function.Function
		args     []cty.Value
		expected cty.Value // cty.NilVal for the error
	}{
		{name: "a logarithm", fn: log, args: []cty.Value{num("100"), num("10")}, expected: num("2")},
		{name: "a power", fn: pow, args: []cty.Value{num("2"), num("10")}, expected: num("1024")},
		{name: "the logarithm of zero", fn: log, args: []cty.Value{num("0"), num("10")}, expected: cty.NegativeInfinity},
		{name: "the logarithm of a negative number", fn: log, args: []cty.Value{num("-1"), num("10")}},
		{name: "a logarithm in base 1", fn: log, args: []cty.Value{num("1"), num("1")}},
		{name: "a logarithm in a negative base", fn: log, args: []cty.Value{num("10"), num("-1")}},
		{name: "a root of a negative number", fn: pow, args: []cty.Value{num("-8"), num("0.5")}},
		{name: "a number not known", fn: pow, args: []cty.Value{cty.UnknownVal(cty.Number), num("2")}, expected: cty.UnknownVal(cty.Number).RefineNotNull()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.fn.Call(tt.args)

			if tt.expected == cty.NilVal {
				const want = "the result is not a number"
				if err == nil || err.Error() != want {
					t.Errorf("got %#v, %v, want the error %q", got, err, want)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if !got.RawEquals(tt.expected) {
				t.Errorf("got %#v, want %#v", got, tt.expected)
			}
		})
	}
}
