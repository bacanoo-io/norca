package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newTestHandler() http.Handler {
	return NewHandler(core.New(storage.NewMemoryBackend()))
}

// do issues a request against the handler and returns the recorder.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doAuth(t, h, method, path, body, "")
}

// doAuth is like do but sets the X-Vault-Token header when token is non-empty.
func doAuth(t *testing.T, h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rec.Body.String())
	}
	return v
}

func TestSealStatusBeforeInit(t *testing.T) {
	rec := do(t, newTestHandler(), "GET", "/v1/sys/seal-status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	st := decode[statusResponse](t, rec)
	if st.Initialized || !st.Sealed {
		t.Fatalf("status = %+v, want uninitialized+sealed", st)
	}
}

func TestInitThenUnsealFlow(t *testing.T) {
	h := newTestHandler()

	// Initialize 3/2.
	rec := do(t, h, "POST", "/v1/sys/init", `{"secret_shares":3,"secret_threshold":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("init status = %d, body=%s", rec.Code, rec.Body.String())
	}
	init := decode[initResponse](t, rec)
	if len(init.Keys) != 3 || len(init.KeysBase64) != 3 {
		t.Fatalf("init keys = %+v", init)
	}

	// Still sealed, now initialized.
	st := decode[statusResponse](t, do(t, h, "GET", "/v1/sys/seal-status", ""))
	if !st.Initialized || !st.Sealed || st.T != 2 || st.N != 3 {
		t.Fatalf("post-init status = %+v", st)
	}

	// First share: still sealed, progress 1.
	body := `{"key":"` + init.Keys[0] + `"}`
	st = decode[statusResponse](t, do(t, h, "POST", "/v1/sys/unseal", body))
	if !st.Sealed || st.Progress != 1 {
		t.Fatalf("after 1 share: %+v", st)
	}

	// Second share: unsealed.
	body = `{"key":"` + init.Keys[1] + `"}`
	st = decode[statusResponse](t, do(t, h, "POST", "/v1/sys/unseal", body))
	if st.Sealed || st.Progress != 0 {
		t.Fatalf("after threshold: %+v", st)
	}
}

func TestUnsealAcceptsBase64Share(t *testing.T) {
	h := newTestHandler()
	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))

	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.KeysBase64[0]+`"}`)
	st := decode[statusResponse](t, do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.KeysBase64[1]+`"}`))
	if st.Sealed {
		t.Fatalf("expected unsealed with base64 shares: %+v", st)
	}
}

func TestSealEndpoint(t *testing.T) {
	h := newTestHandler()
	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[1]+`"}`)

	// Seal now requires authentication.
	rec := doAuth(t, h, "POST", "/v1/sys/seal", "", init.RootToken)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("seal status = %d", rec.Code)
	}
	st := decode[statusResponse](t, do(t, h, "GET", "/v1/sys/seal-status", ""))
	if !st.Sealed {
		t.Fatal("not sealed after POST /v1/sys/seal")
	}
}

func TestInitReturnsRootToken(t *testing.T) {
	rec := do(t, newTestHandler(), "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`)
	init := decode[initResponse](t, rec)
	if init.RootToken == "" {
		t.Fatal("init did not return a root token")
	}
}

func TestInitTwiceIsError(t *testing.T) {
	h := newTestHandler()
	do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`)
	rec := do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("second init status = %d, want 400", rec.Code)
	}
	if e := decode[errorResponse](t, rec); len(e.Errors) == 0 {
		t.Fatal("expected error message")
	}
}

func TestInitInvalidConfig(t *testing.T) {
	rec := do(t, newTestHandler(), "POST", "/v1/sys/init", `{"secret_shares":1,"secret_threshold":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUnsealBadShareEncoding(t *testing.T) {
	h := newTestHandler()
	do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`)
	rec := do(t, h, "POST", "/v1/sys/unseal", `{"key":"not-hex-or-base64!!"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMalformedJSONRejected(t *testing.T) {
	h := newTestHandler()
	rec := do(t, h, "POST", "/v1/sys/init", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	rec := do(t, newTestHandler(), "POST", "/v1/sys/init",
		`{"secret_shares":2,"secret_threshold":2,"bogus":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", rec.Code)
	}
}

func TestContentTypeIsJSON(t *testing.T) {
	rec := do(t, newTestHandler(), "GET", "/v1/sys/seal-status", "")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// Guard: init response body must not be empty (keys returned).
func TestInitReturnsNonEmptyKeys(t *testing.T) {
	rec := do(t, newTestHandler(), "POST", "/v1/sys/init", `{"secret_shares":5,"secret_threshold":3}`)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"keys"`)) {
		t.Fatalf("init body missing keys: %s", rec.Body.String())
	}
}
