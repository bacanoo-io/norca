// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca/rigger/host"
)

// An overwrite that says what it replaces is checked: not over other items,
// and by the index of those it read.
func TestSecrets_EnsureReplaces(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		replaces string
		expected string
	}{
		{name: "changed since", replaces: "before", expected: ""},
		{name: "as read", replaces: "now", expected: "cas=7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var put string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					put = r.URL.RawQuery

					w.WriteHeader(http.StatusInternalServerError)

					return
				}

				w.Header().Set("X-Nomad-Index", "7")
				w.Header().Set("X-Nomad-KnownLeader", "true")
				w.Header().Set("X-Nomad-LastContact", "0")

				body := &api.SecretValue{Secret: &api.Secret{ModifyIndex: 7}, Items: map[string]string{"db": "now"}}
				if err := json.NewEncoder(w).Encode(body); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(srv.Close)

			client, err := api.NewClient(&api.Config{Address: srv.URL})
			if err != nil {
				t.Fatal(err)
			}

			written, err := NewSecrets(client, stackNamespace).Ensure(t.Context(), host.Secret{
				Path: "rigger/x", Overwrite: true,
				Items:    map[string]host.Sensitive{"db": host.NewSensitive("next")},
				Replaces: map[string]host.Sensitive{"db": host.NewSensitive(tt.replaces)},
			})

			switch {
			case written:
				t.Error("Ensure() wrote")
			case tt.expected == "" && (put != "" || err != nil):
				t.Errorf("a changed secret was written to (%q, %v)", put, err)
			case tt.expected != "" && put != tt.expected+"&namespace="+stackNamespace:
				t.Errorf("write = %q, want %s", put, tt.expected)
			}
		})
	}
}
