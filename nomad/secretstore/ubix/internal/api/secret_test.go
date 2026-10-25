package api

import (
	"net/http"
	"testing"
)

// unsealedHandler returns a handler whose vault is initialized and unsealed,
// along with its root token.
func unsealedHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	h := newTestHandler()
	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[1]+`"}`)
	return h, init.RootToken
}

func TestSecretRequiresToken(t *testing.T) {
	h, root := unsealedHandler(t)

	// No token → 401.
	if rec := do(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"}}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	// Bogus token → 403.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"}}`, "uv.not-a-real-token"); rec.Code != http.StatusForbidden {
		t.Fatalf("bad token = %d, want 403", rec.Code)
	}
	// Valid root token → 200.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"}}`, root); rec.Code != http.StatusOK {
		t.Fatalf("root token = %d, want 200", rec.Code)
	}
}

func TestSecretWriteReadRoundTrip(t *testing.T) {
	h, root := unsealedHandler(t)

	rec := doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"user":"admin","pass":"hunter2"}}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("write status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = doAuth(t, h, "GET", "/v1/secret/data/app/db", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decode[map[string]any](t, rec)
	secret := body["data"].(map[string]any)["data"].(map[string]any)
	if secret["user"] != "admin" || secret["pass"] != "hunter2" {
		t.Fatalf("round-trip secret = %v", secret)
	}
}

func TestSecretVersioningOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"v":"1"}}`, root)
	doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"v":"2"}}`, root)

	body := decode[map[string]any](t, doAuth(t, h, "GET", "/v1/secret/data/p", "", root))
	if body["data"].(map[string]any)["data"].(map[string]any)["v"] != "2" {
		t.Fatalf("latest = %v", body)
	}
	body = decode[map[string]any](t, doAuth(t, h, "GET", "/v1/secret/data/p?version=1", "", root))
	if body["data"].(map[string]any)["data"].(map[string]any)["v"] != "1" {
		t.Fatalf("v1 = %v", body)
	}
}

func TestSecretOpsFailWhenSealed(t *testing.T) {
	h := newTestHandler()
	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	// Initialized but sealed. Even with the real root token, the token store
	// cannot be read while sealed, so requests fail with 503.
	rec := doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"}}`, init.RootToken)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("write while sealed = %d, want 503", rec.Code)
	}
}

func TestSecretReadMissing(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "GET", "/v1/secret/data/nope", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("read missing = %d, want 404", rec.Code)
	}
}

func TestSecretSoftDeleteUndelete(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"}}`, root)

	if rec := doAuth(t, h, "DELETE", "/v1/secret/data/p", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/secret/data/p", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete = %d, want 404", rec.Code)
	}
	if rec := doAuth(t, h, "POST", "/v1/secret/undelete/p", `{"versions":[1]}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("undelete = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/secret/data/p", "", root); rec.Code != http.StatusOK {
		t.Fatalf("read after undelete = %d, want 200", rec.Code)
	}
}

func TestSecretDestroy(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"}}`, root)
	if rec := doAuth(t, h, "POST", "/v1/secret/destroy/p", `{"versions":[1]}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("destroy = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/secret/data/p?version=1", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("read destroyed = %d, want 404", rec.Code)
	}
}

func TestSecretMetadataAndList(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/secret/data/team/a", `{"data":{"x":"1"}}`, root)
	doAuth(t, h, "POST", "/v1/secret/data/team/a", `{"data":{"x":"2"}}`, root)
	doAuth(t, h, "POST", "/v1/secret/data/team/b", `{"data":{"x":"1"}}`, root)

	body := decode[map[string]any](t, doAuth(t, h, "GET", "/v1/secret/metadata/team/a", "", root))
	if body["data"].(map[string]any)["current_version"].(float64) != 2 {
		t.Fatalf("metadata current_version = %v", body)
	}

	rec := doAuth(t, h, "LIST", "/v1/secret/metadata/team", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	keys := decode[map[string]any](t, rec)["data"].(map[string]any)["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("list keys = %v, want 2", keys)
	}
}

func TestSecretDeleteMetadataRemovesAll(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"}}`, root)
	if rec := doAuth(t, h, "DELETE", "/v1/secret/metadata/p", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete metadata = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/secret/metadata/p", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("metadata after delete = %d, want 404", rec.Code)
	}
}

func TestSecretWriteAcceptsOptions(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "POST", "/v1/secret/data/p", `{"data":{"a":"b"},"options":{"cas":0}}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("write with options = %d, body=%s", rec.Code, rec.Body.String())
	}
}
