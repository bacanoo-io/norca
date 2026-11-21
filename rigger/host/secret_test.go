// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/host"
)

const secretText = "s3cr3t-value"

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
	secret := host.Secret{Path: "db/admin", Items: map[string]host.Sensitive{"password": s}}

	tests := []struct {
		name   string
		format string
		arg    any
	}{
		{name: "value with v", format: "%v", arg: s},
		{name: "value with plus v", format: "%+v", arg: s},
		{name: "value with sharp v", format: "%#v", arg: s},
		{name: "value with s", format: "%s", arg: s},
		{name: "value with q", format: "%q", arg: s},
		{name: "value with x", format: "%x", arg: s},
		{name: "map of values", format: "%v", arg: secret.Items},
		{name: "secret with plus v", format: "%+v", arg: secret},
		{name: "secret with sharp v", format: "%#v", arg: secret},
		{name: "pointer to secret", format: "%+v", arg: &secret},
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

func TestSensitive_LogValue(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.InfoContext(t.Context(), "secret written", "password", host.NewSensitive(secretText))

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
		{name: "map of values", arg: map[string]host.Sensitive{"password": host.NewSensitive(secretText)}},
		{name: "secret", arg: host.Secret{Items: map[string]host.Sensitive{"password": host.NewSensitive(secretText)}}},
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
