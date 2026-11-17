// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jwt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The error of a login whose keys can't be fetched is shown to whoever
// tried to log in: it holds no credential of the JWKS or discovery URL of
// the auth method.
func TestValidate_ErrorHidesURLCredentials(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	must.NoError(t, err)
	token, _, err := mock.SampleJWTokenWithKeys(nil, rsaKey)
	must.NoError(t, err)

	dead := httptest.NewServer(http.NotFoundHandler())
	u, err := url.Parse(dead.URL)
	must.NoError(t, err)
	dead.Close()
	u.User = url.UserPassword("SECRETTOKEN", "SECRETPASS")
	u.Path = "/keys"
	u.RawQuery = "api_key=SECRETVALUE"

	for name, conf := range map[string]*structs.ACLAuthMethodConfig{
		"jwks":      {JWKSURL: u.String(), SigningAlgs: []string{"RS256"}},
		"discovery": {OIDCDiscoveryURL: u.String(), SigningAlgs: []string{"RS256"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Validate(context.Background(), token, conf)
			must.Error(t, err)
			must.StrNotContains(t, err.Error(), "SECRET")
		})
	}
}
