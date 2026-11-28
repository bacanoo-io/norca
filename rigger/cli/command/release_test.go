// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
)

// appsNamespace is the namespace of the releases of the release tests.
const appsNamespace = "apps"

// testServer answers the test of a release the way the release API does:
// "web" passes, "broken" fails, any other release is not found.
type testServer struct {
	mu       sync.Mutex
	requests []string
}

func (s *testServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())

	var res api.ReleaseTestResult

	switch r.URL.Path {
	case "/v1/release/web/test":
		res = api.ReleaseTestResult{Revision: 3, Succeeded: true, Hooks: []*api.ReleaseHookResult{
			{Phase: cmdTest, Namespace: appsNamespace, Job: "web-smoke", Status: "complete"},
		}}
	case "/v1/release/broken/test":
		res = api.ReleaseTestResult{Revision: 1, Hooks: []*api.ReleaseHookResult{
			{Phase: cmdTest, Namespace: appsNamespace, Job: "broken-smoke", Status: "failed", Message: "exit code 1"},
		}}
	default:
		http.Error(w, `release "missing" not found`, http.StatusNotFound)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(res); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// rigger release test runs the tests of a release and prints each hook
// job; a failed test ends with exit code 1, a release that is not there
// with its error, and --timeout reaches the servers.
//
//nolint:paralleltest // the subtests share the environment of the cluster address (t.Setenv)
func TestReleaseTest(t *testing.T) {
	fake := &testServer{}

	srv := httptest.NewServer(fake)
	defer srv.Close()

	t.Setenv("NORCA_ADDR", srv.URL)
	t.Setenv("NORCA_NAMESPACE", "")

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
		wantURI    string
	}{
		{
			name:       "the tests pass",
			args:       []string{riggerRelease, "--namespace", appsNamespace, "--timeout", "2m"},
			wantCode:   exitOK,
			wantStdout: "test apps/web: revision 3 passed\n  test apps/web-smoke: complete\n",
			wantURI:    "/v1/release/web/test?namespace=apps&timeout=2m0s",
		},
		{
			name:       "a test fails",
			args:       []string{"broken", "--namespace", appsNamespace},
			wantCode:   exitError,
			wantStdout: "test apps/broken: revision 1 failed\n  test apps/broken-smoke: failed (exit code 1)\n",
			wantStderr: errTestFailed.Error(),
			wantURI:    "/v1/release/broken/test?namespace=apps",
		},
		{
			name:       "the release is not there",
			args:       []string{"missing"},
			wantCode:   exitError,
			wantStderr: "not found",
			wantURI:    "/v1/release/missing/test?namespace=default",
		},
		{
			name:       "no name",
			args:       nil,
			wantCode:   exitError,
			wantStderr: errReleaseName.Error(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake.mu.Lock()
			fake.requests = nil
			fake.mu.Unlock()

			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), append([]string{cmdRelease, cmdTest}, tt.args...), &stdout, &stderr)
			if code != tt.wantCode || stdout.String() != tt.wantStdout || !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("release test %v = %d, stdout %q, stderr %q", tt.args, code, stdout.String(), stderr.String())
			}

			fake.mu.Lock()
			defer fake.mu.Unlock()

			got := strings.Join(fake.requests, ", ")
			if tt.wantURI == "" {
				if got != "" {
					t.Errorf("requests %q, want none", got)
				}

				return
			}

			// The release API takes the test as PUT or POST.
			if len(fake.requests) != 1 || !strings.HasSuffix(got, " "+tt.wantURI) {
				t.Errorf("requests %q, want one to %s", got, tt.wantURI)
			}
		})
	}
}

// The help of rigger release test shows --timeout and not --adopt, the
// flag of rollback.
func TestReleaseTest_Help(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{cmdRelease, cmdTest, "--help"}, &stdout, &stderr)
	if code != exitOK || !strings.Contains(stdout.String(), "--timeout") || strings.Contains(stdout.String(), "--adopt") {
		t.Errorf("help = %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
