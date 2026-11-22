// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
)

// The version of a cluster is Norca's, not that of the API it is
// compatible with (Config.Version).
func TestCluster_InfoVersion(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/agent/self") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		body := &api.AgentSelf{
			Config:       map[string]any{"Version": map[string]any{"Version": "1.6.5"}},
			NorcaVersion: "0.3.0",
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	info, err := NewCluster(client).Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if info.Version != "0.3.0" {
		t.Errorf("version = %q, want 0.3.0, that of Norca", info.Version)
	}
}
