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

	"github.com/bacanoo-io/norca/rigger/host"
)

// phaseFailed is the phase of what did not succeed.
const phaseFailed = "failed"

// The status of a current revision is deployed only when the release is
// too: an uninstall that failed leaves the revision as it was.
func TestReleases_CurrentStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, release, revision, expected string
	}{
		{name: "both", release: host.StatusDeployed, revision: host.StatusDeployed, expected: host.StatusDeployed},
		{name: "uninstall failed", release: phaseFailed, revision: host.StatusDeployed, expected: phaseFailed},
		{name: "apply failed", release: phaseFailed, revision: phaseFailed, expected: phaseFailed},
		{name: "revision on the way", release: host.StatusDeployed, revision: "pending", expected: "pending"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body any = &api.Release{
					Name: shop, Spec: &api.ReleaseSpec{Revision: 1}, Status: &api.ReleaseStatus{Phase: tt.release},
				}
				if strings.Contains(r.URL.Path, "/revision/") {
					body = &api.ReleaseRevision{
						Revision: 1, Spec: &api.ReleaseRevisionSpec{}, Status: &api.ReleaseRevisionStatus{Phase: tt.revision},
					}
				}

				w.Header().Set("X-Nomad-Index", "1")
				w.Header().Set("X-Nomad-KnownLeader", "true")
				w.Header().Set("X-Nomad-LastContact", "0")

				if err := json.NewEncoder(w).Encode(body); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(srv.Close)

			client, err := api.NewClient(&api.Config{Address: srv.URL})
			if err != nil {
				t.Fatal(err)
			}

			rev, err := NewReleases(client).Current(t.Context(), host.ReleaseRef{Namespace: stackNamespace, Name: shop})
			if err != nil {
				t.Fatal(err)
			}

			if rev.Status != tt.expected {
				t.Errorf("status = %q, want %q", rev.Status, tt.expected)
			}
		})
	}
}
