package api

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestTransitRewrapOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/orders", "", root)

	rec := doAuth(t, h, "POST", "/v1/transit/encrypt/orders", `{"plaintext":"`+b64("secret")+`"}`, root)
	ct := decode[map[string]any](t, rec)["data"].(map[string]any)["ciphertext"].(string)

	// Rotate, then rewrap the v1 ciphertext to the latest version.
	doAuth(t, h, "POST", "/v1/transit/keys/orders/rotate", "", root)
	rec = doAuth(t, h, "POST", "/v1/transit/rewrap/orders", `{"ciphertext":"`+ct+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("rewrap = %d, body=%s", rec.Code, rec.Body.String())
	}
	ct2 := decode[map[string]any](t, rec)["data"].(map[string]any)["ciphertext"].(string)
	if ct2 == "" || ct2 == ct {
		t.Fatalf("rewrap ciphertext = %q (original %q)", ct2, ct)
	}

	// It still decrypts to the original plaintext.
	rec = doAuth(t, h, "POST", "/v1/transit/decrypt/orders", `{"ciphertext":"`+ct2+`"}`, root)
	pt := decode[map[string]any](t, rec)["data"].(map[string]any)["plaintext"].(string)
	if pt != b64("secret") {
		t.Fatalf("decrypt after rewrap = %q, want %q", pt, b64("secret"))
	}
}

func TestTransitDataKeyPlaintextOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/kek", "", root)

	rec := doAuth(t, h, "POST", "/v1/transit/datakey/plaintext/kek", `{"bits":256}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("datakey = %d, body=%s", rec.Code, rec.Body.String())
	}
	data := decode[map[string]any](t, rec)["data"].(map[string]any)
	ptB64, _ := data["plaintext"].(string)
	wrapped, _ := data["ciphertext"].(string)
	if ptB64 == "" || wrapped == "" {
		t.Fatalf("datakey plaintext=%q ciphertext=%q", ptB64, wrapped)
	}
	raw, err := base64.StdEncoding.DecodeString(ptB64)
	if err != nil || len(raw) != 32 {
		t.Fatalf("plaintext key decode err=%v len=%d, want 32", err, len(raw))
	}

	// Unwrapping the wrapped key yields the same plaintext.
	rec = doAuth(t, h, "POST", "/v1/transit/decrypt/kek", `{"ciphertext":"`+wrapped+`"}`, root)
	if got := decode[map[string]any](t, rec)["data"].(map[string]any)["plaintext"].(string); got != ptB64 {
		t.Fatalf("unwrapped data key = %q, want %q", got, ptB64)
	}
}

func TestTransitDataKeyWrappedHidesPlaintext(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/kek", "", root)

	// Default bits (no body) and wrapped mode must not leak the plaintext key.
	rec := doAuth(t, h, "POST", "/v1/transit/datakey/wrapped/kek", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("datakey wrapped = %d, body=%s", rec.Code, rec.Body.String())
	}
	data := decode[map[string]any](t, rec)["data"].(map[string]any)
	if _, present := data["plaintext"]; present {
		t.Fatal("wrapped datakey response leaked plaintext")
	}
	if data["ciphertext"].(string) == "" {
		t.Fatal("wrapped datakey response has no ciphertext")
	}
}

func TestTransitDataKeyBadMode(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/kek", "", root)
	if rec := doAuth(t, h, "POST", "/v1/transit/datakey/bogus/kek", "", root); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode = %d, want 400", rec.Code)
	}
}
