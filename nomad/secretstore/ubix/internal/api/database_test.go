package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/database"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// fakeDBPlugin is an in-memory database.Plugin for testing the HTTP layer
// without a real database.
type fakeDBPlugin struct {
	created []database.CreateUserRequest
	revoked []string
}

func (f *fakeDBPlugin) Initialize(context.Context, string) error { return nil }
func (f *fakeDBPlugin) CreateUser(_ context.Context, req database.CreateUserRequest) error {
	f.created = append(f.created, req)
	return nil
}
func (f *fakeDBPlugin) RevokeUser(_ context.Context, u string) error {
	f.revoked = append(f.revoked, u)
	return nil
}
func (f *fakeDBPlugin) Close() error { return nil }

// unsealedDBHandler returns an unsealed handler whose database engine uses a
// fake plugin, plus the root token and the fake for inspection.
func unsealedDBHandler(t *testing.T) (*Handler, string, *fakeDBPlugin) {
	t.Helper()
	c := core.New(storage.NewMemoryBackend())
	h := NewHandler(c)
	fake := &fakeDBPlugin{}
	h.database = database.New(c.Barrier(), databaseMountPrefix, fake)

	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[1]+`"}`)
	return h, init.RootToken, fake
}

// configureAndRole configures the engine and writes a role.
func configureAndRole(t *testing.T, h *Handler, root string) {
	t.Helper()
	if rec := doAuth(t, h, "POST", "/v1/database/config", `{"connection_url":"user:pw@tcp(localhost)/db"}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("configure = %d, body=%s", rec.Code, rec.Body.String())
	}
	role := `{"creation_statements":["CREATE USER '{{username}}' IDENTIFIED BY '{{password}}';"],"default_ttl":"1h"}`
	if rec := doAuth(t, h, "POST", "/v1/database/roles/app", role, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write role = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestDatabaseCredsFlow(t *testing.T) {
	h, root, fake := unsealedDBHandler(t)
	configureAndRole(t, h, root)

	// Issue a credential.
	rec := doAuth(t, h, "GET", "/v1/database/creds/app", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("creds = %d, body=%s", rec.Code, rec.Body.String())
	}
	data := decode[map[string]any](t, rec)["data"].(map[string]any)
	if data["username"] == "" || data["password"] == "" || data["lease_id"] == "" {
		t.Fatalf("incomplete credential: %v", data)
	}
	if data["lease_duration"].(float64) != 3600 {
		t.Fatalf("lease_duration = %v, want 3600", data["lease_duration"])
	}
	if len(fake.created) != 1 {
		t.Fatalf("plugin CreateUser calls = %d, want 1", len(fake.created))
	}

	// Revoke the lease.
	leaseID := data["lease_id"].(string)
	if rec := doAuth(t, h, "PUT", "/v1/sys/leases/revoke", `{"lease_id":"`+leaseID+`"}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if len(fake.revoked) != 1 || fake.revoked[0] != data["username"] {
		t.Fatalf("revoked = %v, want [%v]", fake.revoked, data["username"])
	}
}

func TestDatabaseConfigStatus(t *testing.T) {
	h, root, _ := unsealedDBHandler(t)
	// Before config.
	rec := doAuth(t, h, "GET", "/v1/database/config", "", root)
	if decode[map[string]any](t, rec)["data"].(map[string]any)["configured"].(bool) {
		t.Fatal("configured=true before configuring")
	}
	// After config.
	configureAndRole(t, h, root)
	rec = doAuth(t, h, "GET", "/v1/database/config", "", root)
	if !decode[map[string]any](t, rec)["data"].(map[string]any)["configured"].(bool) {
		t.Fatal("configured=false after configuring")
	}
}

func TestDatabaseRoleCRUDOverHTTP(t *testing.T) {
	h, root, _ := unsealedDBHandler(t)
	configureAndRole(t, h, root)

	if rec := doAuth(t, h, "GET", "/v1/database/roles/app", "", root); rec.Code != http.StatusOK {
		t.Fatalf("read role = %d", rec.Code)
	}
	rec := doAuth(t, h, "LIST", "/v1/database/roles", "", root)
	if keys := decode[map[string]any](t, rec)["data"].(map[string]any)["keys"].([]any); len(keys) != 1 {
		t.Fatalf("list roles = %v", keys)
	}
	if rec := doAuth(t, h, "DELETE", "/v1/database/roles/app", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete role = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/database/roles/app", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete = %d, want 404", rec.Code)
	}
}

func TestDatabaseCredsBeforeConfigure(t *testing.T) {
	h, root, _ := unsealedDBHandler(t)
	// Role written but engine not configured.
	rec := doAuth(t, h, "GET", "/v1/database/creds/app", "", root)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("creds before config = %d, want 400", rec.Code)
	}
}

func TestDatabaseCredsUnknownRole(t *testing.T) {
	h, root, _ := unsealedDBHandler(t)
	configureAndRole(t, h, root)
	rec := doAuth(t, h, "GET", "/v1/database/creds/missing", "", root)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown role = %d, want 404", rec.Code)
	}
}

func TestDatabaseInvalidTTL(t *testing.T) {
	h, root, _ := unsealedDBHandler(t)
	rec := doAuth(t, h, "POST", "/v1/database/roles/app",
		`{"creation_statements":["x"],"default_ttl":"not-a-duration"}`, root)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid ttl = %d, want 400", rec.Code)
	}
}

func TestDatabaseRequiresAuth(t *testing.T) {
	h, _, _ := unsealedDBHandler(t)
	if rec := do(t, h, "GET", "/v1/database/creds/app", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
}

func TestLeaseRenewOverHTTP(t *testing.T) {
	h, root, _ := unsealedDBHandler(t)
	configureAndRole(t, h, root)

	lease := decode[map[string]any](t, doAuth(t, h, "GET", "/v1/database/creds/app", "", root))["data"].(map[string]any)["lease_id"].(string)

	rec := doAuth(t, h, "PUT", "/v1/sys/leases/renew", `{"lease_id":"`+lease+`","increment":"2h"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew = %d, body=%s", rec.Code, rec.Body.String())
	}
	ld := decode[map[string]any](t, rec)["data"].(map[string]any)["lease_duration"].(float64)
	if ld < 7100 || ld > 7200 {
		t.Fatalf("renewed lease_duration = %v, want ~7200", ld)
	}
}

// TestTokenRevokeSelfCascades confirms revoking a token also revokes the dynamic
// database credentials it created.
func TestTokenRevokeSelfCascades(t *testing.T) {
	h, root, fake := unsealedDBHandler(t)
	configureAndRole(t, h, root)

	// A policy allowing a scoped token to read creds and revoke itself.
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/app",
		`{"path":{"database/creds/app":{"capabilities":["read"]},"auth/token/revoke-self":{"capabilities":["update"]}}}`, root)
	scoped := createToken(t, h, root, `["app"]`)

	// The scoped token gets a DB credential (lease attributed to it).
	credRec := doAuth(t, h, "GET", "/v1/database/creds/app", "", scoped)
	if credRec.Code != http.StatusOK {
		t.Fatalf("creds = %d, body=%s", credRec.Code, credRec.Body.String())
	}
	if len(fake.created) != 1 {
		t.Fatalf("plugin CreateUser calls = %d, want 1", len(fake.created))
	}

	// Revoke the token — its lease should cascade-revoke.
	if rec := doAuth(t, h, "POST", "/v1/auth/token/revoke-self", "", scoped); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.revoked) != 1 {
		t.Fatalf("plugin RevokeUser calls = %d, want 1 (cascade)", len(fake.revoked))
	}
	// And the token itself no longer works.
	if rec := doAuth(t, h, "GET", "/v1/database/creds/app", "", scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked token still works = %d, want 403", rec.Code)
	}
}
