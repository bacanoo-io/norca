// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
)

// selfPath ends the path of the agent's own endpoint, releasesPath that of
// the list of the releases.
const (
	selfPath     = "/agent/self"
	releasesPath = "/releases"
)

// requestWait is how long a test waits for a request of Info to reach its
// server. The request before it may take long on a machine that scans
// local connections (20 seconds were seen on Windows).
const requestWait = 2 * time.Minute

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

// The release API is a feature of a cluster whose servers have it. An
// answer that does not tell is an error of Info: a cluster must not look
// like one without releases because a request failed.
func TestCluster_InfoFeatures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// status and body are the answer of /v1/releases.
		status   int
		body     string
		expected []string
		// wantErr is in the error of Info; empty for none.
		wantErr string
	}{
		{name: "the servers list the releases", status: http.StatusOK, body: "[]", expected: []string{"releases"}},
		{name: "the servers have no such path", status: http.StatusNotFound, body: "not found"},
		{name: "the servers have no such method", status: http.StatusMethodNotAllowed, body: "not allowed"},
		{
			// The servers answer for the API and refuse the token in the
			// namespace: they have it.
			name: "the token may not list the releases", status: http.StatusForbidden, body: "Permission denied", expected: []string{"releases"},
		},
		{name: "the servers fail", status: http.StatusInternalServerError, body: "no cluster leader", wantErr: "no cluster leader"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, selfPath) {
					writeSelf(t, w)

					return
				}

				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			client, err := api.NewClient(&api.Config{Address: srv.URL})
			if err != nil {
				t.Fatal(err)
			}

			info, err := NewCluster(client).Info(t.Context())

			switch {
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Info() = %+v, %v, want an error with %q", info, err, tt.wantErr)
				}
			case err != nil:
				t.Errorf("Info() = %v", err)
			case !slices.Equal(info.Features, tt.expected):
				t.Errorf("features = %v, want %v", info.Features, tt.expected)
			}
		})
	}
}

// hangingCluster is the client of a cluster that never answers the request
// whose path ends with hangs: it waits until its client gives up, or the
// test ends, and is told on the channel. The other requests are answered as
// /v1/agent/self.
func hangingCluster(t *testing.T, hangs string) (*api.Client, <-chan struct{}) {
	t.Helper()

	release := make(chan struct{})
	hung := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, hangs) {
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

	// Close waits for the requests: one whose client never gave up is let
	// go first.
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	client, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	return client, hung
}

// endsWithin waits for the end of a call, for d at most. A machine that
// stood still for longer than d has both the end and the time when it goes
// on: the end is waited for a little longer, so that it wins.
func endsWithin[T any](done <-chan T, d time.Duration) (T, bool) {
	select {
	case v := <-done:
		return v, true
	case <-time.After(d):
	}

	select {
	case v := <-done:
		return v, true
	case <-time.After(5 * time.Second):
		var none T

		return none, false
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
		{name: "the release API does not answer", hangs: releasesPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client, hung := hangingCluster(t, tt.hangs)

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
			case <-time.After(requestWait):
				t.Fatal("no request reached the server")
			}

			cancel()

			err, ended := endsWithin(done, 10*time.Second)
			if !ended {
				t.Fatal("Info still waits for the cluster after its context was cancelled")
			}

			if !errors.Is(err, context.Canceled) {
				t.Errorf("Info() = %v, want the error of the cancelled context", err)
			}
		})
	}
}
