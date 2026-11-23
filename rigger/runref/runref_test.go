// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package runref_test

import (
	"errors"
	"testing"

	"github.com/bacanoo-io/norca/rigger/runref"
)

func TestReplace(t *testing.T) {
	t.Parallel()

	env := runref.Part{Kind: runref.KindEnv, Name: "NORCA_ALLOC_DIR"}
	secret := runref.Part{Kind: runref.KindSecret, Name: "db/app", Item: "password"}

	tests := []struct {
		name     string
		text     string
		place    runref.Place
		expected string
		err      error
	}{
		{name: "no part", text: "plain", place: runref.PlaceJob, expected: "plain"},
		{name: "env in a job", text: env.String() + "/data", place: runref.PlaceJob, expected: "${NORCA_ALLOC_DIR}/data"},
		{name: "env in a template", text: env.String() + "/data", place: runref.PlaceTemplate, expected: `{{ env "NORCA_ALLOC_DIR" }}/data`},
		{
			name: "a secret in a template", text: "PW=" + secret.String(), place: runref.PlaceTemplate,
			expected: `PW={{ with index (norcaSecret "db/app") "password" }}{{ . }}{{ else }}{{ sprig_fail "the secret db/app has no item password" }}{{ end }}`,
		},
		{name: "a secret in a job", text: secret.String(), place: runref.PlaceJob, err: runref.ErrTemplateOnly},
		{
			name: "a quoted secret", place: runref.PlaceTemplate,
			text:     runref.Part{Kind: runref.KindSecret, Name: "s", Item: "i", Quote: runref.QuoteSQL, Trim: true}.String(),
			expected: `'{{ with index (norcaSecret "s") "i" }}{{ . | trimSpace | replaceAll "'" "''" }}{{ else }}{{ sprig_fail "the secret s has no item i" }}{{ end }}'`,
		},
		{
			name: "a service without its port", place: runref.PlaceTemplate,
			text:     runref.Part{Kind: runref.KindService, Name: "pg", NoPort: true}.String(),
			expected: `{{ with nomadService "pg" }}{{ with index . 0 }}{{ .Address }}{{ end }}{{ end }}`,
		},
		{
			name: "the addresses of a service as a list of a configuration", place: runref.PlaceTemplate,
			text: runref.Part{
				Kind: runref.KindServices, Name: "nats-cluster", Scheme: "nats-route", ExcludeSelf: true,
				Quote: runref.QuoteConf, Lead: ", ", Sep: ", ",
			}.String(),
			expected: `{{ $first := true }}{{ range nomadService "nats-cluster" }}{{ if ne .AllocID (env "NORCA_ALLOC_ID") }}` +
				`{{ if $first }}, {{ else }}, {{ end }}{{ $first = false }}"nats-route://{{ .Address }}:{{ .Port }}"{{ end }}{{ end }}`,
		},
		{
			name: "the addresses of a service in a template", place: runref.PlaceTemplate,
			text: runref.Part{Kind: runref.KindServices, Name: "db"}.String(),
			expected: `{{ $first := true }}{{ range nomadService "db" }}` +
				`{{ if $first }}{{ else }},{{ end }}{{ $first = false }}{{ .Address }}:{{ .Port }}{{ end }}`,
		},
		{name: "malformed", text: "xx", place: runref.PlaceJob, err: runref.ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := runref.Replace(tt.text, func(p runref.Part) (string, error) { return p.Resolve(tt.place) })
			if !errors.Is(err, tt.err) {
				t.Fatalf("Replace() error = %v, want %v", err, tt.err)
			}

			if got != tt.expected {
				t.Errorf("Replace() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// An encoder leaves the quotes of a whole value to Resolve (Quote is
// QuoteConf) and writes its own around a part of a string (QuoteConfEscape):
// every kind in every place keeps that, or the value lands bare in the
// configuration.
func TestPart_Resolve_Quote(t *testing.T) {
	t.Parallel()

	const (
		address = `{{ with nomadService "db" }}{{ with index . 0 }}{{ .Address }}:{{ .Port }}{{ end }}{{ end }}`
		allocID = "NORCA_ALLOC_ID"
	)

	tests := []struct {
		name     string
		part     runref.Part
		place    runref.Place
		expected string
	}{
		{
			name: "a service address as a whole value", place: runref.PlaceTemplate,
			part:     runref.Part{Kind: runref.KindService, Name: "db", Quote: runref.QuoteConf},
			expected: `"` + address + `"`,
		},
		{
			name: "a service address inside a string", place: runref.PlaceTemplate,
			part:     runref.Part{Kind: runref.KindService, Name: "db", Quote: runref.QuoteConfEscape},
			expected: address,
		},
		{
			name: "a service address in a template", place: runref.PlaceTemplate,
			part:     runref.Part{Kind: runref.KindService, Name: "db"},
			expected: address,
		},
		{
			name: "a service address in SQL", place: runref.PlaceTemplate,
			part:     runref.Part{Kind: runref.KindService, Name: "db", Quote: runref.QuoteSQL},
			expected: `'` + address + `'`,
		},
		{
			name: "an environment variable as a whole value outside a template", place: runref.PlaceJob,
			part:     runref.Part{Kind: runref.KindEnv, Name: allocID, Quote: runref.QuoteConf},
			expected: `"${NORCA_ALLOC_ID}"`,
		},
		{
			name: "an environment variable inside a string outside a template", place: runref.PlaceJob,
			part:     runref.Part{Kind: runref.KindEnv, Name: allocID, Quote: runref.QuoteConfEscape},
			expected: `${NORCA_ALLOC_ID}`,
		},
		{
			name: "an environment variable in SQL outside a template", place: runref.PlaceJob,
			part:     runref.Part{Kind: runref.KindEnv, Name: allocID, Quote: runref.QuoteSQL},
			expected: `'${NORCA_ALLOC_ID}'`,
		},
		{
			name: "an environment variable without quotes outside a template", place: runref.PlaceJob,
			part:     runref.Part{Kind: runref.KindEnv, Name: allocID, Quote: runref.QuoteNone},
			expected: `${NORCA_ALLOC_ID}`,
		},
		{
			name: "an environment variable as a whole value in a template", place: runref.PlaceTemplate,
			part:     runref.Part{Kind: runref.KindEnv, Name: allocID, Quote: runref.QuoteConf},
			expected: `"{{ env "NORCA_ALLOC_ID" | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\"" }}"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.part.Resolve(tt.place)
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.expected {
				t.Errorf("Resolve() = %q, want %q", got, tt.expected)
			}
		})
	}
}
