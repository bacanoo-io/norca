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

// recordingServer answers every request with the body and records the
// method and the request URI of each.
func recordingServer(t *testing.T, body string) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.RequestURI)
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	conf := DefaultConfig()
	conf.Address = srv.URL
	conf.Region = ""
	conf.Namespace = ""
	c, err := NewClient(conf)
	must.NoError(t, err)
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
}

// A character of the secret path that has a meaning in a URL stays part of
// the path: the request names the secret the caller named, which the server
// refuses, instead of another secret.
func TestSecrets_PathIsEscaped(t *testing.T) {
	testutil.Parallel(t)
	c, requests := recordingServer(t, `{}`)

	_, _, err := c.Secrets().Write("prod/db#staging", &SecretWrite{}, nil)
	must.NoError(t, err)
	_, _, err = c.Secrets().Delete("prod/db#x", nil, nil)
	must.NoError(t, err)
	_, _, err = c.Secrets().Write("prod/db?x", &SecretWrite{}, nil)
	must.NoError(t, err)
	_, _, err = c.Secrets().Read("prod/db?version=3", 0, nil)
	must.NoError(t, err)
	_, _, err = c.Secrets().Read("prod/100%", 0, nil)
	must.NoError(t, err)
	_, _, err = c.Secrets().Read("/prod/db/", 2, nil)
	must.NoError(t, err)
	_, _, err = c.Secrets().Info("prod/db", nil)
	must.NoError(t, err)

	must.Eq(t, []string{
		"PUT /v1/secret/prod/db%23staging",
		"DELETE /v1/secret/prod/db%23x",
		"PUT /v1/secret/prod/db%3Fx",
		"GET /v1/secret/prod/db%3Fversion=3",
		"GET /v1/secret/prod/100%25",
		"GET /v1/secret/prod/db?version=2",
		"GET /v1/secret/prod/db?metadata=",
	}, requests())
}

// A secret path with an empty, "." or ".." segment is refused before a
// request is sent: an HTTP server or a proxy resolves such segments, and the
// request would reach another secret than the one the caller named (review
// finding SEC-24).
func TestSecrets_DotSegmentsRefused(t *testing.T) {
	testutil.Parallel(t)

	// a ServeMux, as the agent has: it cleans the path and redirects
	var mu sync.Mutex
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/secret/", func(w http.ResponseWriter, r *http.Request) {
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
	s := c.Secrets()

	calls := map[string]func(path string) error{
		"Info": func(p string) error { _, _, err := s.Info(p, nil); return err },
		"Read": func(p string) error { _, _, err := s.Read(p, 2, nil); return err },
		"Write": func(p string) error {
			_, _, err := s.Write(p, &SecretWrite{}, nil)
			return err
		},
		"CheckedWrite": func(p string) error {
			_, _, err := s.CheckedWrite(p, 0, &SecretWrite{}, nil)
			return err
		},
		"Delete":   func(p string) error { _, _, err := s.Delete(p, nil, nil); return err },
		"Undelete": func(p string) error { _, _, err := s.Undelete(p, nil, nil); return err },
		"Destroy":  func(p string) error { _, _, err := s.Destroy(p, nil, nil); return err },
	}
	for _, path := range []string{
		"prod/../admin", "../admin", "prod/db/..", "prod/./db", ".", "..", "prod//db", "", "/",
	} {
		for name, call := range calls {
			err := call(path)
			must.ErrorContains(t, err, "invalid secret path", must.Sprintf("%s(%q)", name, path))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	must.SliceEmpty(t, got)
}
