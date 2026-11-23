// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// NATS reads a key in quotes as it is, with no escapes, up to the quote
// that ends it: a key is written between the quotes it does not hold.
func TestNATSConf_Keys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		key      string
		expected string // the key as written
		err      string
	}{
		{name: "a name", key: "max_payload", expected: "max_payload"},
		{name: "a space", key: "my users", expected: `"my users"`},
		{name: "a point", key: "orders.>", expected: `"orders.>"`},
		{name: "a digit first", key: "1st", expected: `"1st"`},
		{name: "a backslash", key: `a\b`, expected: `"a\b"`},
		{name: "a single quote", key: "it's", expected: `"it's"`},
		{name: "a double quote", key: `a"b`, expected: `'a"b'`},
		{name: "both quotes", key: `it's "a"`, err: `it's "a": the key holds both quotes, and NATS reads no escape in a key`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := natsconf.Call([]cty.Value{cty.ObjectVal(map[string]cty.Value{tt.key: cty.NumberIntVal(1)})})

			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if got, want := out.AsString(), tt.expected+": 1\n"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}
