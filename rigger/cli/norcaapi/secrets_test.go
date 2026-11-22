// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca/rigger/host"
)

// A conflict is "made meanwhile" only when the secret is there.
func TestSecrets_EnsureConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "secret not found", http.StatusNotFound)

			return
		}

		http.Error(w, "written by another region", http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	client, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	secret := host.Secret{Path: "rigger/x", Items: map[string]host.Sensitive{"token": host.NewSensitive("one")}}

	if written, err := NewSecrets(client, stackNamespace).Ensure(t.Context(), secret); written || err == nil {
		t.Errorf("Ensure() = %v, %v: a write that failed was taken for a secret that is there", written, err)
	}
}
