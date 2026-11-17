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
