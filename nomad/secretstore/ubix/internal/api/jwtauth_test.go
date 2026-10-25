package api

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"testing"
	"time"
)

func jwtB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signRS256 builds a compact RS256 JWT over claims signed by priv.
func signRS256(t *testing.T, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	pb, _ := json.Marshal(claims)
	signingInput := jwtB64(hb) + "." + jwtB64(pb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + jwtB64(sig)
}

// configureJWT stands up the JWT auth method with a static RSA key and a role
// bound to audience "vault". Returns the handler, root token, and signing key.
func configureJWT(t *testing.T) (http.Handler, string, *rsa.PrivateKey) {
	t.Helper()
	h, root := unsealedHandler(t)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	cfg, _ := json.Marshal(map[string]any{
		"jwt_validation_pubkeys": []string{pubPEM},
		"bound_issuer":           "https://issuer.test",
	})
	if rec := doAuth(t, h, "POST", "/v1/auth/jwt/config", string(cfg), root); rec.Code != http.StatusNoContent {
		t.Fatalf("jwt config = %d, body=%s", rec.Code, rec.Body.String())
	}
	role := `{"bound_audiences":["vault"],"policies":["app-ro"]}`
	if rec := doAuth(t, h, "POST", "/v1/auth/jwt/role/web", role, root); rec.Code != http.StatusNoContent {
		t.Fatalf("jwt role = %d, body=%s", rec.Code, rec.Body.String())
	}
	return h, root, priv
}

// configureJWTWithGroups is configureJWT but with the config's groups_claim set
// to "groups", so a login's asserted groups feed identity external groups.
func configureJWTWithGroups(t *testing.T) (http.Handler, string, *rsa.PrivateKey) {
	t.Helper()
	h, root := unsealedHandler(t)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	cfg, _ := json.Marshal(map[string]any{
		"jwt_validation_pubkeys": []string{pubPEM},
		"bound_issuer":           "https://issuer.test",
		"groups_claim":           "groups",
	})
	if rec := doAuth(t, h, "POST", "/v1/auth/jwt/config", string(cfg), root); rec.Code != http.StatusNoContent {
		t.Fatalf("jwt config = %d, body=%s", rec.Code, rec.Body.String())
	}
	role := `{"bound_audiences":["vault"],"policies":["app-ro"]}`
	if rec := doAuth(t, h, "POST", "/v1/auth/jwt/role/web", role, root); rec.Code != http.StatusNoContent {
		t.Fatalf("jwt role = %d, body=%s", rec.Code, rec.Body.String())
	}
	return h, root, priv
}

func jwtClaims() map[string]any {
	return map[string]any{
		"iss": "https://issuer.test",
		"aud": "vault",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
		"sub": "alice",
	}
}

func TestJWTLoginSuccess(t *testing.T) {
	h, _, priv := configureJWT(t)

	body, _ := json.Marshal(map[string]any{"role": "web", "jwt": signRS256(t, priv, jwtClaims())})
	rec := do(t, h, "POST", "/v1/auth/jwt/login", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, body=%s", rec.Code, rec.Body.String())
	}
	auth := decode[map[string]any](t, rec)["auth"].(map[string]any)
	if pols := auth["policies"].([]any); len(pols) != 1 || pols[0] != "app-ro" {
		t.Fatalf("policies = %v, want [app-ro]", pols)
	}
}

func TestJWTLoginExpired(t *testing.T) {
	h, _, priv := configureJWT(t)
	claims := jwtClaims()
	claims["exp"] = float64(time.Now().Add(-time.Hour).Unix())

	body, _ := json.Marshal(map[string]any{"role": "web", "jwt": signRS256(t, priv, claims)})
	if rec := do(t, h, "POST", "/v1/auth/jwt/login", string(body)); rec.Code != http.StatusForbidden {
		t.Fatalf("expired login = %d, want 403", rec.Code)
	}
}

func TestJWTConfigRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	if rec := do(t, h, "POST", "/v1/auth/jwt/config", `{"bound_issuer":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("config without token = %d, want 401", rec.Code)
	}
}

func TestJWTIssuedTokenWorks(t *testing.T) {
	h, root, priv := configureJWT(t)
	doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"pw":"s3cr3t"}}`, root)
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/app-ro",
		`{"path":{"secret/data/app/db":{"capabilities":["read"]}}}`, root)

	body, _ := json.Marshal(map[string]any{"role": "web", "jwt": signRS256(t, priv, jwtClaims())})
	rec := do(t, h, "POST", "/v1/auth/jwt/login", string(body))
	tok := decode[map[string]any](t, rec)["auth"].(map[string]any)["client_token"].(string)

	if rec := doAuth(t, h, "GET", "/v1/secret/data/app/db", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("issued token read = %d, want 200", rec.Code)
	}
	if rec := doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"x":"y"}}`, tok); rec.Code != http.StatusForbidden {
		t.Fatalf("issued token write = %d, want 403 (read-only policy)", rec.Code)
	}
}
