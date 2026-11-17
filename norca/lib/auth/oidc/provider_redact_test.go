// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oidc

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// The error of a provider that can't be reached is shown to whoever tries
// to log in: it holds no credential of the discovery URL of the auth method.
func TestProviderCache_ErrorHidesURLCredentials(t *testing.T) {
	testCache := NewProviderCache()
	defer testCache.Shutdown()

	dead := httptest.NewServer(http.NotFoundHandler())
	u, err := url.Parse(dead.URL)
	must.NoError(t, err)
	dead.Close()
	u.User = url.UserPassword("SECRETTOKEN", "SECRETPASS")
	u.RawQuery = "api_key=SECRETVALUE"

	authMethod := structs.ACLAuthMethod{
		Name:          "test-oidc-auth-method",
		Type:          "OIDC",
		TokenLocality: "global",
		MaxTokenTTL:   100 * time.Hour,
		Config: &structs.ACLAuthMethodConfig{
			OIDCDiscoveryURL:    u.String(),
			OIDCClientID:        "alice",
			OIDCClientSecret:    "ssshhhh",
			AllowedRedirectURIs: []string{"http://example.com"},
			SigningAlgs:         []string{"RS256"},
		},
	}
	authMethod.SetHash()

	_, err = testCache.Get(&authMethod)
	must.Error(t, err)
	must.StrNotContains(t, err.Error(), "SECRET")
}
