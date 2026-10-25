package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestSnapshotRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	if rec := do(t, h, "POST", "/v1/sys/snapshot", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("snapshot without token = %d, want 401", rec.Code)
	}
}

func TestSnapshotStreamsData(t *testing.T) {
	h, root := unsealedHandler(t)
	// Seed a secret so the snapshot has content.
	doAuth(t, h, "POST", "/v1/secret/data/app", `{"data":{"k":"v"}}`, root)

	rec := doAuth(t, h, "POST", "/v1/sys/snapshot", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "ubixvault-snapshot v1") {
		t.Fatalf("snapshot missing header: %q", body[:min(30, len(body))])
	}
	// The snapshot includes the barrier keyring and the secret's storage keys,
	// but never the plaintext value.
	if !strings.Contains(body, "core/keyring") {
		t.Fatal("snapshot missing keyring entry")
	}
	if strings.Contains(body, `"k":"v"`) || strings.Contains(body, "\"k\":\"v\"") {
		t.Fatal("plaintext secret leaked into snapshot")
	}
}
