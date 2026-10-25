package api

import (
	"net/http"
	"testing"
)

func TestTransitHMACVerifyOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/sig", "", root)

	rec := doAuth(t, h, "POST", "/v1/transit/hmac/sig", `{"input":"`+b64("authenticate-me")+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("hmac = %d, body=%s", rec.Code, rec.Body.String())
	}
	mac := decode[map[string]any](t, rec)["data"].(map[string]any)["hmac"].(string)
	if mac == "" {
		t.Fatal("empty hmac")
	}

	// Correct input verifies.
	rec = doAuth(t, h, "POST", "/v1/transit/verify/sig", `{"input":"`+b64("authenticate-me")+`","hmac":"`+mac+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d, body=%s", rec.Code, rec.Body.String())
	}
	if valid := decode[map[string]any](t, rec)["data"].(map[string]any)["valid"].(bool); !valid {
		t.Fatal("verify of correct input returned valid=false")
	}

	// Tampered input does not.
	rec = doAuth(t, h, "POST", "/v1/transit/verify/sig", `{"input":"`+b64("something-else")+`","hmac":"`+mac+`"}`, root)
	if valid := decode[map[string]any](t, rec)["data"].(map[string]any)["valid"].(bool); valid {
		t.Fatal("verify of tampered input returned valid=true")
	}
}

func TestTransitVerifyRequiresHMAC(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/transit/keys/sig", "", root)
	if rec := doAuth(t, h, "POST", "/v1/transit/verify/sig", `{"input":"`+b64("x")+`"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("verify without hmac = %d, want 400", rec.Code)
	}
}
