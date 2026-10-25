package api

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestTransitEncryptDecryptOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)

	// Create a key.
	if rec := doAuth(t, h, "POST", "/v1/transit/keys/orders", "", root); rec.Code != http.StatusOK {
		t.Fatalf("create key = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Encrypt.
	rec := doAuth(t, h, "POST", "/v1/transit/encrypt/orders", `{"plaintext":"`+b64("card-4242")+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("encrypt = %d, body=%s", rec.Code, rec.Body.String())
	}
	ct := decode[map[string]any](t, rec)["data"].(map[string]any)["ciphertext"].(string)
	if ct == "" {
		t.Fatal("empty ciphertext")
	}

	// Decrypt.
	rec = doAuth(t, h, "POST", "/v1/transit/decrypt/orders", `{"ciphertext":"`+ct+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("decrypt = %d, body=%s", rec.Code, rec.Body.String())
	}
	pt := decode[map[string]any](t, rec)["data"].(map[string]any)["plaintext"].(string)
	if pt != b64("card-4242") {
		t.Fatalf("decrypt plaintext = %q, want %q", pt, b64("card-4242"))
	}
}

func TestTransitKeyLifecycleOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/k", "", root)

	// Read.
	if rec := doAuth(t, h, "GET", "/v1/transit/keys/k", "", root); rec.Code != http.StatusOK {
		t.Fatalf("read key = %d", rec.Code)
	}
	// Rotate.
	if rec := doAuth(t, h, "POST", "/v1/transit/keys/k/rotate", "", root); rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d", rec.Code)
	}
	// List.
	rec := doAuth(t, h, "LIST", "/v1/transit/keys", "", root)
	keys := decode[map[string]any](t, rec)["data"].(map[string]any)["keys"].([]any)
	if len(keys) != 1 || keys[0] != "k" {
		t.Fatalf("list = %v", keys)
	}
	// Delete.
	if rec := doAuth(t, h, "DELETE", "/v1/transit/keys/k", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/transit/keys/k", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete = %d, want 404", rec.Code)
	}
}

func TestTransitRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	if rec := do(t, h, "POST", "/v1/transit/keys/k", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
}

func TestTransitEncryptBadPlaintext(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/k", "", root)
	rec := doAuth(t, h, "POST", "/v1/transit/encrypt/k", `{"plaintext":"not-base64!!"}`, root)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad plaintext = %d, want 400", rec.Code)
	}
}

func TestTransitEncryptUnknownKey(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "POST", "/v1/transit/encrypt/nope", `{"plaintext":"`+b64("x")+`"}`, root)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key = %d, want 404", rec.Code)
	}
}
