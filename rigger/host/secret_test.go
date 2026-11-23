// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/host"
)

const secretText = "s3cr3t-value"

// The item of the secrets of the tests, and the verb that names fields.
const (
	passwordItem = "password"
	verbFields   = "%+v"
)

func TestSensitive_Reveal(t *testing.T) {
	t.Parallel()

	if got := host.NewSensitive(secretText).Reveal(); got != secretText {
		t.Errorf("Reveal() = %q, want %q", got, secretText)
	}

	if got := (host.Sensitive{}).Reveal(); got != "" {
		t.Errorf("Reveal() of the zero value = %q, want empty", got)
	}
}

func TestSensitive_Formatting(t *testing.T) {
	t.Parallel()

	s := host.NewSensitive(secretText)
	secret := host.Secret{Path: "db/admin", Items: map[string]host.Sensitive{passwordItem: s}}

	tests := []struct {
		name   string
		format string
		arg    any
	}{
		{name: "value with v", format: "%v", arg: s},
		{name: "value with plus v", format: verbFields, arg: s},
		{name: "value with sharp v", format: "%#v", arg: s},
		{name: "value with s", format: "%s", arg: s},
		{name: "value with q", format: "%q", arg: s},
		{name: "value with x", format: "%x", arg: s},
		{name: "map of values", format: "%v", arg: secret.Items},
		{name: "secret with plus v", format: verbFields, arg: secret},
		{name: "secret with sharp v", format: "%#v", arg: secret},
		{name: "pointer to secret", format: verbFields, arg: &secret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := fmt.Sprintf(tt.format, tt.arg)
			if strings.Contains(got, secretText) {
				t.Errorf("Sprintf(%q) = %q: shows the value", tt.format, got)
			}
		})
	}
}

// No verb of the fmt package shows the value. For a verb that is none of a
// string fmt prints the fields of a struct, and where it can't call the
// methods of a type (a field that is not exported, a verb it refuses for
// the whole argument: %p of a struct, %w outside Errorf) it prints them by
// reflection.
func TestSensitive_Format(t *testing.T) {
	t.Parallel()

	s := host.NewSensitive(secretText)
	secret := host.Secret{Path: "db/admin", Items: map[string]host.Sensitive{passwordItem: s}}

	args := []struct {
		name string
		arg  any
		// exact is set where the output is the mark and nothing else.
		exact bool
	}{
		{name: "value", arg: s, exact: true},
		{name: "pointer to value", arg: &s},
		{name: "map of values", arg: secret.Items},
		{name: "slice of values", arg: []host.Sensitive{s}},
		{name: "array of values", arg: [1]host.Sensitive{s}},
		{name: "secret", arg: secret},
		{name: "pointer to secret", arg: &secret},
		{name: "field that is not exported", arg: struct{ hidden host.Sensitive }{s}},
		{name: "pointer in a field that is not exported", arg: struct{ hidden *host.Sensitive }{&s}},
	}

	formats := []string{
		"%v", verbFields, "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%c", "%e", "%E", "%f", "%g", "%b", "%o", "%O", "%U",
		"%p", "%w", "%z", "%10v", "%-10d", "%010d", "% x", "%+q", "%#x", "%.3s", "%5.2f",
	}

	// What the value reads as in the forms fmt writes a string in. The hex
	// form is long enough to be no part of an address by chance.
	shown := []string{secretText, secretText[:3], hex.EncodeToString([]byte(secretText)), hex.EncodeToString([]byte(secretText[:8]))}

	for _, a := range args {
		for _, format := range formats {
			t.Run(a.name+" with "+format, func(t *testing.T) {
				t.Parallel()

				got := fmt.Sprintf(format, a.arg)

				for _, part := range shown {
					if strings.Contains(strings.ToLower(strings.ReplaceAll(got, " ", "")), part) {
						t.Errorf("Sprintf(%q) = %q: shows the value", format, got)

						break
					}
				}

				if a.exact && format != "%p" && format != "%w" && got != "(sensitive)" {
					t.Errorf("Sprintf(%q) = %q, want (sensitive)", format, got)
				}
			})
		}
	}
}

// The zero value and a copy read as the value they are of.
func TestSensitive_Copies(t *testing.T) {
	t.Parallel()

	s := host.NewSensitive(secretText)
	c := s

	if c.Reveal() != secretText || s.Reveal() != secretText {
		t.Errorf("a copy reveals %q, its source %q", c.Reveal(), s.Reveal())
	}

	var zero host.Sensitive
	if got := fmt.Sprintf("%v %d", zero, zero); got != "(sensitive) (sensitive)" {
		t.Errorf("the zero value prints as %q", got)
	}

	if !(host.Secret{Replaces: map[string]host.Sensitive{"a": s}}).ReplacesOnly(map[string]host.Sensitive{"a": host.NewSensitive(secretText)}) {
		t.Error("two values of the same text are not the same item")
	}
}

func TestSensitive_LogValue(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.InfoContext(t.Context(), "secret written", passwordItem, host.NewSensitive(secretText))

	if strings.Contains(buf.String(), secretText) {
		t.Errorf("log line %q shows the value", buf.String())
	}

	if !strings.Contains(buf.String(), "(sensitive)") {
		t.Errorf("log line %q does not mark the value as sensitive", buf.String())
	}
}

func TestSensitive_MarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		arg  any
	}{
		{name: "value", arg: host.NewSensitive(secretText)},
		{name: "map of values", arg: map[string]host.Sensitive{passwordItem: host.NewSensitive(secretText)}},
		{name: "secret", arg: host.Secret{Items: map[string]host.Sensitive{passwordItem: host.NewSensitive(secretText)}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.arg)
			if !errors.Is(err, host.ErrSensitive) {
				t.Errorf("json.Marshal() error = %v, want ErrSensitive", err)
			}

			if bytes.Contains(data, []byte(secretText)) {
				t.Errorf("json.Marshal() = %s: shows the value", data)
			}
		})
	}
}

func TestSensitive_MarshalText(t *testing.T) {
	t.Parallel()

	data, err := host.NewSensitive(secretText).MarshalText()
	if !errors.Is(err, host.ErrSensitive) {
		t.Errorf("MarshalText() error = %v, want ErrSensitive", err)
	}

	if data != nil {
		t.Errorf("MarshalText() = %q, want nil", data)
	}
}
