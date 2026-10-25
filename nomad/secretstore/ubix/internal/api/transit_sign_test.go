package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestTransitSignVerifyOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)

	// Create an ECDSA signing key; the response carries its public key.
	rec := doAuth(t, h, "POST", "/v1/transit/keys/signer", `{"type":"ecdsa-p256"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("create signing key = %d, body=%s", rec.Code, rec.Body.String())
	}
	data := decode[map[string]any](t, rec)["data"].(map[string]any)
	if data["type"] != "ecdsa-p256" {
		t.Fatalf("key type = %v", data["type"])
	}
	if pks, _ := data["public_keys"].(map[string]any); pks == nil || !strings.Contains(pks["1"].(string), "PUBLIC KEY") {
		t.Fatalf("expected public key in create response, got %v", data["public_keys"])
	}

	// Sign.
	rec = doAuth(t, h, "POST", "/v1/transit/sign/signer", `{"input":"`+b64("artifact")+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign = %d, body=%s", rec.Code, rec.Body.String())
	}
	sig := decode[map[string]any](t, rec)["data"].(map[string]any)["signature"].(string)
	if sig == "" {
		t.Fatal("empty signature")
	}

	// Verify correct + tampered.
	rec = doAuth(t, h, "POST", "/v1/transit/verify/signer", `{"input":"`+b64("artifact")+`","signature":"`+sig+`"}`, root)
	if valid := decode[map[string]any](t, rec)["data"].(map[string]any)["valid"].(bool); !valid {
		t.Fatal("verify of correct input returned valid=false")
	}
	rec = doAuth(t, h, "POST", "/v1/transit/verify/signer", `{"input":"`+b64("forged")+`","signature":"`+sig+`"}`, root)
	if valid := decode[map[string]any](t, rec)["data"].(map[string]any)["valid"].(bool); valid {
		t.Fatal("verify of tampered input returned valid=true")
	}
}

func TestTransitSignOnSymmetricKeyOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/sym", "", root) // default aes256
	if rec := doAuth(t, h, "POST", "/v1/transit/sign/sym", `{"input":"`+b64("x")+`"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("sign on symmetric key = %d, want 400", rec.Code)
	}
}

func TestTransitVerifyRequiresExactlyOne(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/signer", `{"type":"ed25519"}`, root)
	// Neither hmac nor signature.
	if rec := doAuth(t, h, "POST", "/v1/transit/verify/signer", `{"input":"`+b64("x")+`"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("verify with neither = %d, want 400", rec.Code)
	}
	// Both.
	body := `{"input":"` + b64("x") + `","hmac":"ubix:v1:AAAA","signature":"ubix:v1:AAAA"}`
	if rec := doAuth(t, h, "POST", "/v1/transit/verify/signer", body, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("verify with both = %d, want 400", rec.Code)
	}
}
