// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestAuthHost_Credential(t *testing.T) {
	ci.Parallel(t)
	o := &Options{Username: "bot", Password: "tok", AuthHost: "registry.example.com", NoDockerConfig: true, NoEnvCredentials: true}
	cred, err := o.credential()(context.Background(), "registry.example.com:443")
	must.NoError(t, err)
	must.Eq(t, "bot", cred.Username)
	cred, err = o.credential()(context.Background(), "auth.other.example:443")
	must.NoError(t, err)
	must.Eq(t, auth.EmptyCredential, cred, must.Sprint("no credentials for other hosts"))

	// The servers never use credentials of their environment.
	t.Run("no ambient credentials", func(t *testing.T) {
		o := &Options{NoDockerConfig: true, NoEnvCredentials: true}
		must.Nil(t, o.credential())
	})
}

func TestAuthHost_Transport(t *testing.T) {
	ci.Parallel(t)
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Host+" "+r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	tr := &authHostTransport{host: u.Host, next: http.DefaultTransport}
	for _, target := range []string{srv.URL + "/v2/", "http://localhost:" + u.Port() + "/token"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Basic Ym90OnRvaw==")
		resp, err := tr.RoundTrip(req)
		must.NoError(t, err)
		resp.Body.Close()
	}
	must.Eq(t, []string{u.Host + " Basic Ym90OnRvaw==", "localhost:" + u.Port() + " "}, got)
}
