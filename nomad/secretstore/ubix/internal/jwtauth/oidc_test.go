package jwtauth

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
)

// discoveryFetch returns a fetch func that serves the OIDC discovery document at
// <issuer>/.well-known/openid-configuration (pointing jwks_uri at <issuer>/keys)
// and the RSA JWKS at <issuer>/keys. calls counts discovery-document fetches.
func discoveryFetch(t *testing.T, issuer string, jwks []byte, calls *int) func(context.Context, string) ([]byte, error) {
	t.Helper()
	discovery, _ := json.Marshal(map[string]any{
		"issuer":   issuer,
		"jwks_uri": issuer + "/keys",
	})
	return func(_ context.Context, url string) ([]byte, error) {
		switch url {
		case issuer + "/.well-known/openid-configuration":
			*calls++
			return discovery, nil
		case issuer + "/keys":
			return jwks, nil
		}
		return nil, fmt.Errorf("unexpected fetch url %q", url)
	}
}

func TestLoginViaOIDCDiscovery(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	priv, _ := genRSA(t)

	eBytes := big.NewInt(int64(priv.E)).Bytes()
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": "k1", "alg": "RS256",
		"n": b64(priv.N.Bytes()), "e": b64(eBytes),
	}}})

	calls := 0
	m.fetch = discoveryFetch(t, "https://issuer.test", jwks, &calls)

	if err := m.Configure(ctx, Config{OIDCDiscoveryURL: "https://issuer.test", BoundIssuer: "https://issuer.test"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := m.WriteRole(ctx, "web", Role{BoundAudiences: []string{"vault"}, Policies: []string{"p"}}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}

	if _, err := m.Login(ctx, "web", makeRS256(t, priv, stdClaims())); err != nil {
		t.Fatalf("first login via discovery: %v", err)
	}
	// A second login must reuse the discovered jwks_uri (no re-discovery).
	if _, err := m.Login(ctx, "web", makeRS256(t, priv, stdClaims())); err != nil {
		t.Fatalf("second login: %v", err)
	}
	if calls != 1 {
		t.Fatalf("discovery document fetched %d times, want 1 (cached)", calls)
	}
}

func TestOIDCDiscoveryFailureDenies(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	priv, _ := genRSA(t)
	// Discovery always fails.
	m.fetch = func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("network down") }

	if err := m.Configure(ctx, Config{OIDCDiscoveryURL: "https://issuer.test", BoundIssuer: "https://issuer.test"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := m.WriteRole(ctx, "web", Role{Policies: []string{"p"}}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	if _, err := m.Login(ctx, "web", makeRS256(t, priv, stdClaims())); err == nil {
		t.Fatal("login should be denied when discovery fails")
	}
}

func TestConfigureAcceptsDiscoveryURLAlone(t *testing.T) {
	if err := newMethod(t).Configure(context.Background(), Config{OIDCDiscoveryURL: "https://issuer.test"}); err != nil {
		t.Fatalf("Configure with only oidc_discovery_url: %v", err)
	}
}
