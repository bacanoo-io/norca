// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/api/internal/testutil"
	"github.com/shoenig/test/must"
)

// An identifier that is "." or "..", or empty before the end of the path, is
// refused before a request is sent: the HTTP server or a proxy resolves the
// path it makes into the path of another endpoint or object (review finding
// SEC-27).
func TestClient_DotSegmentsRefused(t *testing.T) {
	testutil.Parallel(t)

	// a ServeMux, as the agent has: it cleans the path and redirects
	var mu sync.Mutex
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	conf := DefaultConfig()
	conf.Address = srv.URL
	conf.Region = ""
	conf.Namespace = ""
	c, err := NewClient(conf)
	must.NoError(t, err)

	calls := map[string]func(id string) error{
		"Jobs.Deregister": func(id string) error {
			_, _, err := c.Jobs().Deregister(id, true, nil)
			return err
		},
		"Jobs.Info": func(id string) error {
			_, _, err := c.Jobs().Info(id, nil)
			return err
		},
		"Evaluations.Allocations": func(id string) error {
			_, _, err := c.Evaluations().Allocations(id, nil)
			return err
		},
		"ACLPolicies.Info": func(id string) error {
			_, _, err := c.ACLPolicies().Info(id, nil)
			return err
		},
		"SecretEngines.Info": func(id string) error {
			_, _, err := c.SecretEngines().Info(id, nil)
			return err
		},
	}
	for _, id := range []string{"..", "."} {
		for name, call := range calls {
			err := call(id)
			must.ErrorContains(t, err, "invalid request path", must.Sprintf("%s(%q)", name, id))
		}
	}
	// an empty identifier inside the path: /v1/evaluation//allocations
	must.ErrorContains(t, calls["Evaluations.Allocations"](""), "invalid request path")
	_, _, err = c.Jobs().Scale("", "group", pointerOf(1), "", false, nil, nil)
	must.ErrorContains(t, err, "invalid request path")
	mu.Lock()
	must.SliceEmpty(t, got)
	mu.Unlock()

	// ordinary identifiers, also escaped ones, are sent as they are, and an
	// empty one at the end is left to the server
	_, _, err = c.Jobs().Info("web/api", nil)
	must.NoError(t, err)
	_, _, err = c.Jobs().Info("a..b", nil)
	must.NoError(t, err)
	_, _, err = c.Jobs().Info("", nil)
	must.NoError(t, err)
	mu.Lock()
	must.Eq(t, []string{"GET /v1/job/web/api", "GET /v1/job/a..b", "GET /v1/job/"}, got)
	mu.Unlock()
}
