package jwtauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newMethod(t *testing.T) *Method {
	t.Helper()
	mem := storage.NewMemoryBackend()
	return New(mem, token.NewStore(mem), "auth/jwt")
}

func genRSA(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genRSA: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal pub: %v", err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return priv, pemStr
}

func makeRS256(t *testing.T, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	pb, _ := json.Marshal(claims)
	signingInput := b64(hb) + "." + b64(pb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + b64(sig)
}

func stdClaims() map[string]any {
	return map[string]any{
		"iss":  "https://issuer.test",
		"aud":  "vault",
		"exp":  float64(time.Now().Add(time.Hour).Unix()),
		"sub":  "alice",
		"role": "engineer",
	}
}

// configured returns a method with a static RSA key and a role bound to the
// audience "vault" and the claim role=engineer.
func configured(t *testing.T) (*Method, *rsa.PrivateKey) {
	ctx := context.Background()
	m := newMethod(t)
	priv, pemStr := genRSA(t)
	if err := m.Configure(ctx, Config{ValidationPubKeys: []string{pemStr}, BoundIssuer: "https://issuer.test"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := m.WriteRole(ctx, "web", Role{
		BoundAudiences: []string{"vault"},
		BoundClaims:    map[string]string{"role": "engineer"},
		Policies:       []string{"readers"},
		TokenTTL:       time.Hour,
	}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	return m, priv
}

func TestLoginStaticKey(t *testing.T) {
	ctx := context.Background()
	m, priv := configured(t)

	tok, err := m.Login(ctx, "web", makeRS256(t, priv, stdClaims()))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(tok.Policies) != 1 || tok.Policies[0] != "readers" {
		t.Fatalf("policies = %v", tok.Policies)
	}
	if tok.ExpiresAt.IsZero() {
		t.Fatal("token should expire (TokenTTL set)")
	}
}

func TestLoginRejections(t *testing.T) {
	ctx := context.Background()
	m, priv := configured(t)

	cases := map[string]func(map[string]any){
		"expired":        func(c map[string]any) { c["exp"] = float64(time.Now().Add(-time.Hour).Unix()) },
		"wrong issuer":   func(c map[string]any) { c["iss"] = "https://evil.test" },
		"wrong audience": func(c map[string]any) { c["aud"] = "someone-else" },
		"claim mismatch": func(c map[string]any) { c["role"] = "intern" },
		"missing exp":    func(c map[string]any) { delete(c, "exp") },
	}
	for name, mutate := range cases {
		c := stdClaims()
		mutate(c)
		if _, err := m.Login(ctx, "web", makeRS256(t, priv, c)); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: want ErrDenied, got %v", name, err)
		}
	}

	// A signature from a different key must be rejected.
	other, _ := genRSA(t)
	if _, err := m.Login(ctx, "web", makeRS256(t, other, stdClaims())); !errors.Is(err, ErrDenied) {
		t.Errorf("wrong signing key: want ErrDenied, got %v", err)
	}
	// A tampered payload invalidates the signature.
	valid := makeRS256(t, priv, stdClaims())
	if _, err := m.Login(ctx, "web", valid[:len(valid)-4]+"AAAA"); !errors.Is(err, ErrDenied) {
		t.Errorf("tampered signature: want ErrDenied, got %v", err)
	}
}

func TestLoginViaJWKS(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	priv, _ := genRSA(t)

	// Serve a JWKS containing the public key via the injectable fetch.
	eBytes := big.NewInt(int64(priv.E)).Bytes()
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": "k1", "alg": "RS256",
		"n": b64(priv.N.Bytes()), "e": b64(eBytes),
	}}})
	m.fetch = func(context.Context, string) ([]byte, error) { return jwks, nil }

	if err := m.Configure(ctx, Config{JWKSURL: "https://issuer.test/jwks", BoundIssuer: "https://issuer.test"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := m.WriteRole(ctx, "web", Role{BoundAudiences: []string{"vault"}, Policies: []string{"p"}}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	if _, err := m.Login(ctx, "web", makeRS256(t, priv, stdClaims())); err != nil {
		t.Fatalf("JWKS login: %v", err)
	}
}

// TestResetDropsJWKSCache: after Reset (a seal) the next login fetches the JWKS
// again instead of trusting keys cached before the seal.
func TestResetDropsJWKSCache(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	priv, _ := genRSA(t)
	eBytes := big.NewInt(int64(priv.E)).Bytes()
	jwks, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": "k1", "alg": "RS256",
		"n": b64(priv.N.Bytes()), "e": b64(eBytes),
	}}})
	fetches := 0
	m.fetch = func(context.Context, string) ([]byte, error) { fetches++; return jwks, nil }
	if err := m.Configure(ctx, Config{JWKSURL: "https://issuer.test/jwks", BoundIssuer: "https://issuer.test"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := m.WriteRole(ctx, "web", Role{BoundAudiences: []string{"vault"}, Policies: []string{"p"}}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}

	for range 2 {
		if _, err := m.Login(ctx, "web", makeRS256(t, priv, stdClaims())); err != nil {
			t.Fatalf("Login: %v", err)
		}
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d after two logins, want 1 (cached)", fetches)
	}
	m.Reset()
	if _, err := m.Login(ctx, "web", makeRS256(t, priv, stdClaims())); err != nil {
		t.Fatalf("Login after Reset: %v", err)
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d after Reset, want 2", fetches)
	}
}

func TestNotConfigured(t *testing.T) {
	m := newMethod(t)
	_ = m.WriteRole(context.Background(), "web", Role{Policies: []string{"p"}})
	if _, err := m.Login(context.Background(), "web", "a.b.c"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestExtractGroups(t *testing.T) {
	cases := []struct {
		name   string
		claim  string
		claims map[string]any
		want   []string
	}{
		{"array", "groups", map[string]any{"groups": []any{"a", "b"}}, []string{"a", "b"}},
		{"single string", "groups", map[string]any{"groups": "a"}, []string{"a"}},
		{"array with non-strings", "groups", map[string]any{"groups": []any{"a", 1, "", "b"}}, []string{"a", "b"}},
		{"missing claim", "groups", map[string]any{"sub": "x"}, nil},
		{"empty claim name", "", map[string]any{"groups": []any{"a"}}, nil},
		{"wrong type", "groups", map[string]any{"groups": 42}, nil},
	}
	for _, c := range cases {
		got := extractGroups(c.claims, c.claim)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}
