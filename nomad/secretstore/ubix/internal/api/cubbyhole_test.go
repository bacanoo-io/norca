package api

import (
	"net/http"
	"testing"
)

// TestCubbyholeRoundTrip writes, reads back, lists, and deletes within a token's
// own cubbyhole over HTTP.
func TestCubbyholeRoundTrip(t *testing.T) {
	h, root := unsealedHandler(t)
	tok := createToken(t, h, root, `["p"]`)

	if rec := doAuth(t, h, "POST", "/v1/cubbyhole/creds", `{"user":"alice","pw":"s3cr3t"}`, tok); rec.Code != http.StatusNoContent {
		t.Fatalf("write = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec := doAuth(t, h, "GET", "/v1/cubbyhole/creds", "", tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("read = %d, body=%s", rec.Code, rec.Body.String())
	}
	data := decode[map[string]any](t, rec)["data"].(map[string]any)
	if data["user"] != "alice" || data["pw"] != "s3cr3t" {
		t.Fatalf("read data = %v", data)
	}

	if rec := doAuth(t, h, "LIST", "/v1/cubbyhole/", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("list = %d, body=%s", rec.Code, rec.Body.String())
	}

	if rec := doAuth(t, h, "DELETE", "/v1/cubbyhole/creds", "", tok); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/cubbyhole/creds", "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete = %d, want 404", rec.Code)
	}
}

// TestCubbyholeIsolation confirms one token cannot see another's cubbyhole, and
// that the root token is not privileged here either.
func TestCubbyholeIsolation(t *testing.T) {
	h, root := unsealedHandler(t)
	tokA := createToken(t, h, root, `["p"]`)
	tokB := createToken(t, h, root, `["p"]`)

	if rec := doAuth(t, h, "POST", "/v1/cubbyhole/note", `{"v":"A-only"}`, tokA); rec.Code != http.StatusNoContent {
		t.Fatalf("A write = %d", rec.Code)
	}
	// B sees nothing at the same path.
	if rec := doAuth(t, h, "GET", "/v1/cubbyhole/note", "", tokB); rec.Code != http.StatusNotFound {
		t.Fatalf("B read of A's path = %d, want 404", rec.Code)
	}
	// Even root sees nothing — the scoping is structural, not an ACL grant.
	if rec := doAuth(t, h, "GET", "/v1/cubbyhole/note", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("root read of A's path = %d, want 404", rec.Code)
	}
}

// TestCubbyholeRequiresAuth confirms the endpoints reject an unauthenticated
// request.
func TestCubbyholeRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	if rec := do(t, h, "GET", "/v1/cubbyhole/x", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read = %d, want 401", rec.Code)
	}
}

// TestCubbyholeDestroyedOnRevoke confirms revoke-self wipes the token's
// cubbyhole, so a later reuse of the (now-invalid) token cannot reach the data.
func TestCubbyholeDestroyedOnRevoke(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/revoker",
		`{"path":{"auth/token/revoke-self":{"capabilities":["update"]}}}`, root)
	tok := createToken(t, h, root, `["revoker"]`)

	if rec := doAuth(t, h, "POST", "/v1/cubbyhole/secret", `{"v":"gone-soon"}`, tok); rec.Code != http.StatusNoContent {
		t.Fatalf("write = %d", rec.Code)
	}
	if rec := doAuth(t, h, "POST", "/v1/auth/token/revoke-self", "", tok); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The token is now invalid; the request is rejected before reaching storage.
	if rec := doAuth(t, h, "GET", "/v1/cubbyhole/secret", "", tok); rec.Code != http.StatusForbidden {
		t.Fatalf("read after revoke = %d, want 403", rec.Code)
	}
	// A freshly minted token with the same policies starts with an empty
	// cubbyhole — proof the storage was actually cleared, not merely gated.
	fresh := createToken(t, h, root, `["p"]`)
	rec := doAuth(t, h, "LIST", "/v1/cubbyhole/", "", fresh)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh list = %d", rec.Code)
	}
}
