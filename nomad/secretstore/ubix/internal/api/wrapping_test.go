package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrapUnwrapOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)

	// Wrap a payload with a 1-minute TTL (via the X-Vault-Wrap-TTL header).
	req := httptest.NewRequest("POST", "/v1/sys/wrapping/wrap", strings.NewReader(`{"password":"s3cr3t"}`))
	req.Header.Set("X-Vault-Token", root)
	req.Header.Set(wrapTTLHeader, "1m")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("wrap = %d, body=%s", rec.Code, rec.Body.String())
	}
	info := decode[map[string]any](t, rec)["wrap_info"].(map[string]any)
	token, _ := info["token"].(string)
	if token == "" {
		t.Fatalf("no wrap token: %v", info)
	}

	// Unwrap returns the original payload.
	rec = doAuth(t, h, "POST", "/v1/sys/wrapping/unwrap", `{"token":"`+token+`"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("unwrap = %d, body=%s", rec.Code, rec.Body.String())
	}
	data := decode[map[string]any](t, rec)["data"].(map[string]any)
	if data["password"] != "s3cr3t" {
		t.Fatalf("unwrapped data = %v, want password=s3cr3t", data)
	}

	// A second unwrap of the same token fails (single-use).
	if rec := doAuth(t, h, "POST", "/v1/sys/wrapping/unwrap", `{"token":"`+token+`"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("second unwrap = %d, want 400", rec.Code)
	}
}

func TestWrapRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	if rec := do(t, h, "POST", "/v1/sys/wrapping/wrap", `{"k":"v"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrap without token = %d, want 401", rec.Code)
	}
}

func TestUnwrapUnknownTokenOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/sys/wrapping/unwrap", `{"token":"uvw.deadbeef"}`, root); rec.Code != http.StatusBadRequest {
		t.Fatalf("unwrap unknown = %d, want 400", rec.Code)
	}
}

func TestWrapDefaultTTLOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	// No X-Vault-Wrap-TTL header -> default TTL, still wraps.
	rec := doAuth(t, h, "POST", "/v1/sys/wrapping/wrap", `{"k":"v"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("wrap (default ttl) = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ttl := decode[map[string]any](t, rec)["wrap_info"].(map[string]any)["ttl"].(float64); ttl <= 0 {
		t.Fatalf("default ttl = %v, want > 0", ttl)
	}
}
