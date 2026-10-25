package api

import (
	"net/http"
	"testing"
)

// TestTransitDerivedConvergentHTTP exercises a convergent derived key over HTTP:
// same plaintext+context encrypts deterministically, a context is required, and
// decryption needs the matching context.
func TestTransitDerivedConvergentHTTP(t *testing.T) {
	h, root := unsealedHandler(t)

	if rec := doAuth(t, h, "POST", "/v1/transit/keys/conv", `{"convergent":true}`, root); rec.Code != http.StatusOK {
		t.Fatalf("create convergent key = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Read-key reports the flags.
	info := decode[map[string]any](t, doAuth(t, h, "GET", "/v1/transit/keys/conv", "", root))["data"].(map[string]any)
	if info["convergent"] != true || info["derived"] != true {
		t.Fatalf("key info = %v, want derived+convergent", info)
	}

	pt := b64("card-4242")
	dctx := b64("tenant-a")
	body := `{"plaintext":"` + pt + `","context":"` + dctx + `"}`

	c1 := transitCiphertext(t, h, root, body)
	c2 := transitCiphertext(t, h, root, body)
	if c1 != c2 {
		t.Fatalf("convergent HTTP encryption not deterministic:\n c1=%s\n c2=%s", c1, c2)
	}

	// Decrypt with the matching context works.
	rec := doAuth(t, h, "POST", "/v1/transit/decrypt/conv", `{"ciphertext":"`+c1+`","context":"`+dctx+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("decrypt = %d, body=%s", rec.Code, rec.Body.String())
	}
	dec := decode[map[string]any](t, rec)["data"].(map[string]any)
	if dec["plaintext"] != pt {
		t.Fatalf("decrypt plaintext = %v, want %v", dec["plaintext"], pt)
	}
	// Decrypt with a wrong context fails.
	if rec := doAuth(t, h, "POST", "/v1/transit/decrypt/conv",
		`{"ciphertext":"`+c1+`","context":"`+b64("tenant-b")+`"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("decrypt wrong context = %d, want 400", rec.Code)
	}
	// Encrypt without a context on a derived key is a 400.
	if rec := doAuth(t, h, "POST", "/v1/transit/encrypt/conv", `{"plaintext":"`+pt+`"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("encrypt without context = %d, want 400", rec.Code)
	}
}

// TestTransitContextRejectedForPlainKey confirms a context on a normal key is a
// 400.
func TestTransitContextRejectedForPlainKey(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/plain", "", root)
	if rec := doAuth(t, h, "POST", "/v1/transit/encrypt/plain",
		`{"plaintext":"`+b64("x")+`","context":"`+b64("ctx")+`"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("context on plain key = %d, want 400", rec.Code)
	}
}

// transitCiphertext posts an encrypt request and returns the ciphertext,
// failing on a non-200.
func transitCiphertext(t *testing.T, h http.Handler, root, body string) string {
	t.Helper()
	rec := doAuth(t, h, "POST", "/v1/transit/encrypt/conv", body, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("encrypt = %d, body=%s", rec.Code, rec.Body.String())
	}
	return decode[map[string]any](t, rec)["data"].(map[string]any)["ciphertext"].(string)
}
