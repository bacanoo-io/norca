package api

import (
	"net/http"
	"testing"
)

// makeUserAndLogin writes a userpass user with the given policies and logs in,
// returning the auth block from the login response.
func makeUserAndLogin(t *testing.T, h http.Handler, root, user, pw, policiesJSON string) map[string]any {
	t.Helper()
	if rec := doAuth(t, h, "POST", "/v1/auth/userpass/users/"+user, `{"password":"`+pw+`","policies":`+policiesJSON+`}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write user = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec := do(t, h, "POST", "/v1/auth/userpass/login/"+user, `{"password":"`+pw+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, body=%s", rec.Code, rec.Body.String())
	}
	return decode[map[string]any](t, rec)["auth"].(map[string]any)
}

// TestLoginAutoCreatesEntity confirms a userpass login gets an entity stamped on
// its token, and a second login for the same user resolves to the same entity.
func TestLoginAutoCreatesEntity(t *testing.T) {
	h, root := unsealedHandler(t)

	auth1 := makeUserAndLogin(t, h, root, "alice", "pw1", `["base"]`)
	entity := auth1["entity_id"].(string)
	if entity == "" {
		t.Fatal("login produced no entity_id")
	}

	// A second login for alice resolves to the same entity.
	rec := do(t, h, "POST", "/v1/auth/userpass/login/alice", `{"password":"pw1"}`)
	auth2 := decode[map[string]any](t, rec)["auth"].(map[string]any)
	if auth2["entity_id"].(string) != entity {
		t.Fatalf("second login entity = %s, want %s", auth2["entity_id"], entity)
	}

	// The auto-created entity is listed and readable.
	list := decode[map[string]any](t, doAuth(t, h, "LIST", "/v1/identity/entity/id", "", root))["data"].(map[string]any)
	if len(list["keys"].([]any)) != 1 {
		t.Fatalf("entity list = %v, want one", list["keys"])
	}
	ent := decode[map[string]any](t, doAuth(t, h, "GET", "/v1/identity/entity/id/"+entity, "", root))["data"].(map[string]any)
	if ent["name"].(string) != "userpass/alice" {
		t.Fatalf("auto-created entity name = %v, want userpass/alice", ent["name"])
	}
}

// TestEntityPolicyGrantsAcrossToken is the headline Phase-1 behavior: a policy
// attached to the entity grants access the login's own role did not, and it
// takes effect for a token minted *before* the entity policy existed (resolution
// is per-request, not snapshotted at login).
func TestEntityPolicyGrantsAcrossToken(t *testing.T) {
	h, root := unsealedHandler(t)

	// A policy that allows reading one KV path.
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/kv-reader",
		`{"path":{"secret/data/shared":{"capabilities":["read","create","update"]}}}`, root)

	// alice logs in with NO useful policy; her token can't touch secret/data/shared.
	auth := makeUserAndLogin(t, h, root, "alice", "pw", `["base"]`)
	tok := auth["client_token"].(string)
	entity := auth["entity_id"].(string)
	if rec := doAuth(t, h, "POST", "/v1/secret/data/shared", `{"data":{"k":"v"}}`, tok); rec.Code != http.StatusForbidden {
		t.Fatalf("pre-grant write = %d, want 403", rec.Code)
	}

	// Attach the policy to her entity (addressed by ID, since the auto-created
	// name contains "/") — no re-login.
	if rec := doAuth(t, h, "POST", "/v1/identity/entity",
		`{"id":"`+entity+`","policies":["kv-reader"]}`, root); rec.Code != http.StatusOK {
		t.Fatalf("update entity = %d, body=%s", rec.Code, rec.Body.String())
	}

	// The same, already-issued token now has access via the entity policy.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/shared", `{"data":{"k":"v"}}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("post-grant write = %d, body=%s, want 200", rec.Code, rec.Body.String())
	}
	if rec := doAuth(t, h, "GET", "/v1/secret/data/shared", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("post-grant read = %d, want 200", rec.Code)
	}
}

// TestEntityAliasLinksTwoLogins confirms an explicit alias makes a second
// auth-method login resolve to an existing entity, so both logins share its
// policies.
func TestEntityAliasLinksTwoLogins(t *testing.T) {
	h, root := unsealedHandler(t)

	auth := makeUserAndLogin(t, h, root, "bob", "pw", `["base"]`)
	entity := auth["entity_id"].(string)

	// Bind an approle-side login name to the same entity.
	rec := doAuth(t, h, "POST", "/v1/identity/entity-alias",
		`{"name":"bob-role","canonical_id":"`+entity+`","mount_type":"approle"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("create alias = %d, body=%s", rec.Code, rec.Body.String())
	}
	aliasID := decode[map[string]any](t, rec)["data"].(map[string]any)["id"].(string)
	if aliasID == "" {
		t.Fatal("alias got no id")
	}

	// Deleting the alias succeeds.
	if rec := doAuth(t, h, "DELETE", "/v1/identity/entity-alias/id/"+aliasID, "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete alias = %d", rec.Code)
	}
}

// TestIdentityRequiresAuthz confirms a non-root token without an identity grant
// cannot manage entities.
func TestIdentityRequiresAuthz(t *testing.T) {
	h, root := unsealedHandler(t)
	scoped := createToken(t, h, root, `["base"]`)
	if rec := doAuth(t, h, "POST", "/v1/identity/entity", `{"name":"x"}`, scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized entity write = %d, want 403", rec.Code)
	}
}
