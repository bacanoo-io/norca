// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package runref_test

import (
	"errors"
	"testing"

	"github.com/bacanoo-io/norca-rig/runref"
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
			text: runref.Part{Kind: runref.KindServices, Name: "nats-cluster", Scheme: "nats-route", ExcludeSelf: true, Quote: runref.QuoteConf}.String(),
			expected: `{{ $first := true }}{{ range nomadService "nats-cluster" }}{{ if ne .AllocID (env "NORCA_ALLOC_ID") }}` +
				"{{ if not $first }}\n{{ end }}{{ $first = false }}" + `"nats-route://{{ .Address }}:{{ .Port }}"{{ end }}{{ end }}`,
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
