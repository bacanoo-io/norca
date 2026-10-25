package api

import (
	"net/http"
	"strings"
	"testing"
)

func sealStatusSealed(t *testing.T, h http.Handler) bool {
	t.Helper()
	rec := do(t, h, "GET", "/v1/sys/seal-status", "")
	return decode[map[string]any](t, rec)["sealed"].(bool)
}

func TestRekeyOverHTTP(t *testing.T) {
	h := newTestHandler()
	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	oldKeys := init.Keys
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+oldKeys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+oldKeys[1]+`"}`)

	// Start a rekey to 3 shares / threshold 2.
	rec := do(t, h, "POST", "/v1/sys/rekey/init", `{"secret_shares":3,"secret_threshold":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rekey init = %d, body=%s", rec.Code, rec.Body.String())
	}
	st := decode[rekeyResponse](t, rec)
	if !st.Started || st.Nonce == "" || st.Required != 2 || st.NewShares != 3 {
		t.Fatalf("rekey init status = %+v", st)
	}

	// Feed the current shares; the second completes it and returns 3 new shares.
	do(t, h, "POST", "/v1/sys/rekey/update", `{"nonce":"`+st.Nonce+`","key":"`+oldKeys[0]+`"}`)
	rec = do(t, h, "POST", "/v1/sys/rekey/update", `{"nonce":"`+st.Nonce+`","key":"`+oldKeys[1]+`"}`)
	done := decode[rekeyResponse](t, rec)
	if !done.Complete || len(done.Keys) != 3 {
		t.Fatalf("rekey completion = %+v", done)
	}
	newKeys := done.Keys

	// Seal, then confirm the OLD shares no longer unseal.
	if rec := doAuth(t, h, "POST", "/v1/sys/seal", "", init.RootToken); rec.Code != http.StatusNoContent {
		t.Fatalf("seal = %d", rec.Code)
	}
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+oldKeys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+oldKeys[1]+`"}`)
	if !sealStatusSealed(t, h) {
		t.Fatal("old shares unsealed the vault after rekey")
	}

	// The NEW shares do unseal it.
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+newKeys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+newKeys[1]+`"}`)
	if sealStatusSealed(t, h) {
		t.Fatal("new shares did not unseal the vault")
	}
}

func TestRekeyInitRequiresUnsealed(t *testing.T) {
	h := newTestHandler()
	decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	// Initialized but still sealed.
	if rec := do(t, h, "POST", "/v1/sys/rekey/init", `{"secret_shares":3,"secret_threshold":2}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rekey init while sealed = %d, want 503", rec.Code)
	}
}

func TestRekeyStatusEmpty(t *testing.T) {
	h, _ := unsealedHandler(t)
	rec := do(t, h, "GET", "/v1/sys/rekey/init", "")
	if st := decode[rekeyResponse](t, rec); st.Started {
		t.Fatalf("expected no rekey in progress, got %+v", st)
	}
}

func TestRekeyCancelOverHTTP(t *testing.T) {
	h, _ := unsealedHandler(t) // 2/2, unsealed
	rec := do(t, h, "POST", "/v1/sys/rekey/init", `{"secret_shares":3,"secret_threshold":2}`)
	nonce := decode[rekeyResponse](t, rec).Nonce
	if rec := do(t, h, "DELETE", "/v1/sys/rekey/init", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("rekey cancel = %d", rec.Code)
	}
	// Updating a cancelled attempt is a bad request. Use a valid-length (33-byte)
	// share so this exercises the "no attempt in progress" path, not length validation.
	validLenKey := strings.Repeat("ab", 33)
	if rec := do(t, h, "POST", "/v1/sys/rekey/update", `{"nonce":"`+nonce+`","key":"`+validLenKey+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("update after cancel = %d, want 400", rec.Code)
	}
}
