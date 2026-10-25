package api

import (
	"net/http"
	"testing"
)

// initUnsealedShamir initializes and unseals a 3/2 Shamir vault, returning the
// handler and the hex unseal keys.
func initUnsealedShamir(t *testing.T) (http.Handler, []string) {
	t.Helper()
	h := newTestHandler()
	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":3,"secret_threshold":2}`))
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[1]+`"}`)
	return h, init.Keys
}

func TestGenerateRootFlowOverHTTP(t *testing.T) {
	h, keys := initUnsealedShamir(t)

	// init (unauthenticated).
	rec := do(t, h, "POST", "/v1/sys/generate-root/init", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("init = %d, body=%s", rec.Code, rec.Body.String())
	}
	nonce := decode[rootGenResponse](t, rec).Nonce
	if nonce == "" {
		t.Fatal("no nonce returned")
	}

	// first share → not complete.
	st := decode[rootGenResponse](t, do(t, h, "POST", "/v1/sys/generate-root/update",
		`{"nonce":"`+nonce+`","key":"`+keys[0]+`"}`))
	if st.Complete || st.Progress != 1 {
		t.Fatalf("after 1 share: %+v", st)
	}

	// second share → complete with a new root token.
	st = decode[rootGenResponse](t, do(t, h, "POST", "/v1/sys/generate-root/update",
		`{"nonce":"`+nonce+`","key":"`+keys[2]+`"}`))
	if !st.Complete || st.RootToken == "" {
		t.Fatalf("final: %+v", st)
	}

	// The new root token actually works (root can seal).
	if rec := doAuth(t, h, "POST", "/v1/sys/seal", "", st.RootToken); rec.Code != http.StatusNoContent {
		t.Fatalf("new root token seal = %d, want 204", rec.Code)
	}
}

func TestGenerateRootWrongNonceOverHTTP(t *testing.T) {
	h, keys := initUnsealedShamir(t)
	do(t, h, "POST", "/v1/sys/generate-root/init", "")
	rec := do(t, h, "POST", "/v1/sys/generate-root/update", `{"nonce":"wrong","key":"`+keys[0]+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong nonce = %d, want 400", rec.Code)
	}
}

func TestGenerateRootCancelOverHTTP(t *testing.T) {
	h, _ := initUnsealedShamir(t)
	do(t, h, "POST", "/v1/sys/generate-root/init", "")
	if rec := do(t, h, "DELETE", "/v1/sys/generate-root/init", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d, want 204", rec.Code)
	}
	st := decode[rootGenResponse](t, do(t, h, "GET", "/v1/sys/generate-root/attempt", ""))
	if st.Started {
		t.Fatalf("attempt still active: %+v", st)
	}
}

func TestGenerateRootUnauthenticated(t *testing.T) {
	h, _ := initUnsealedShamir(t)
	// No token required — the share quorum is the authority.
	if rec := do(t, h, "POST", "/v1/sys/generate-root/init", ""); rec.Code != http.StatusOK {
		t.Fatalf("init without token = %d, want 200", rec.Code)
	}
}
