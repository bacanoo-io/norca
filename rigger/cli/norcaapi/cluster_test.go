// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
)

// selfPath ends the path of the agent's own endpoint.
const selfPath = "/agent/self"

// writeSelf answers /v1/agent/self as an agent of Norca 0.3.0.
func writeSelf(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	body := &api.AgentSelf{
		Config:       map[string]any{"Version": map[string]any{"Version": "1.6.5"}},
		NorcaVersion: "0.3.0",
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Error(err)
	}
}

// The version of a cluster is Norca's, not that of the API it is
// compatible with (Config.Version).
func TestCluster_InfoVersion(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, selfPath) {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		writeSelf(t, w)
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

// Info ends when its context is done, whichever of its two requests waits
// for the cluster.
func TestCluster_Info_Cancelled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// hangs ends the path of the request that is never answered.
		hangs string
	}{
		{name: "the agent does not answer", hangs: selfPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			release := make(chan struct{})
			hung := make(chan struct{}, 1)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, tt.hangs) {
					writeSelf(t, w)

					return
				}

				select {
				case hung <- struct{}{}:
				default:
				}

				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))

			// Close waits for the requests: one whose client never gave
			// up is let go first.
			t.Cleanup(func() {
				close(release)
				srv.Close()
			})

			client, err := api.NewClient(&api.Config{Address: srv.URL})
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)

			go func() {
				_, err := NewCluster(client).Info(ctx)
				done <- err
			}()

			select {
			case <-hung:
			case err := <-done:
				t.Fatalf("Info() = %v before a request hung", err)
			case <-time.After(30 * time.Second):
				t.Fatal("no request reached the server")
			}

			cancel()

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Info() = %v, want the error of the cancelled context", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Info still waits for the cluster after its context was cancelled")
			}
		})
	}
}
