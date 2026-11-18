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

// An identifier with "?", "#" or "%" stays part of the path: the request
// names the object the caller named, instead of another one, or failing
// to parse (review finding SEC-28).
func TestClient_IdentifierIsEscaped(t *testing.T) {
	testutil.Parallel(t)
	c, requests := recordingServer(t, `{}`)

	_, _, err := c.ACLPolicies().Info("ops?x=1", nil)
	must.NoError(t, err)
	_, _, err = c.Nodes().Info("node%1", nil)
	must.NoError(t, err)
	// the recording server answers {}, not a list: only the request counts
	_, _, _ = c.Evaluations().Allocations("eval#1", nil)
	_, err = c.Namespaces().Delete("team?a", nil)
	must.NoError(t, err)
	_, _, err = c.Deployments().Fail("dep?1", nil)
	must.NoError(t, err)

	must.Eq(t, []string{
		"GET /v1/acl/policy/ops%3Fx=1",
		"GET /v1/node/node%251",
		"GET /v1/evaluation/eval%231/allocations",
		"DELETE /v1/namespace/team%3Fa",
		"PUT /v1/deployment/fail/dep%3F1",
	}, requests())
}

// A node ID with "&" or "#" stays one query value: it does not add
// parameters or cut the query (review finding SEC-28, query values).
func TestNodes_QueryValueIsEscaped(t *testing.T) {
	testutil.Parallel(t)
	c, requests := recordingServer(t, `{}`)

	// the recording server answers {}, not a list: only the request counts
	_, _ = c.Nodes().CSIVolumes("n1&type=host", nil)
	_, _ = c.Nodes().Stats("n2#x", nil)
	_ = c.Nodes().GC("n3&node_id=n4", nil)

	must.Eq(t, []string{
		"GET /v1/volumes?node_id=n1%26type%3Dhost&type=csi",
		"GET /v1/client/stats?node_id=n2%23x",
		"GET /v1/client/gc?node_id=n3%26node_id%3Dn4",
	}, requests())
}
