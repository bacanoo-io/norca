package api

import (
	"net/http"
	"testing"
)

// TestTemplatedPolicySelfServiceSubtree is the phase-4 headline: one templated
// policy lets each user reach only their own KV subtree, keyed by their entity
// name — no per-user policy.
func TestTemplatedPolicySelfServiceSubtree(t *testing.T) {
	h, root := unsealedHandler(t)

	// One policy, templated on the entity name, attached to every user's login.
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/self-kv",
		`{"path":{"secret/data/users/{{identity.entity.name}}/*":{"capabilities":["create","read","update"]}}}`, root)

	aliceTok := makeUserAndLogin(t, h, root, "alice", "pw", `["self-kv"]`)["client_token"].(string)
	bobTok := makeUserAndLogin(t, h, root, "bob", "pw", `["self-kv"]`)["client_token"].(string)

	// alice's entity name is "userpass/alice"; her subtree is
	// secret/data/users/userpass/alice/*.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/users/userpass/alice/db", `{"data":{"k":"v"}}`, aliceTok); rec.Code != http.StatusOK {
		t.Fatalf("alice write to own subtree = %d, body=%s, want 200", rec.Code, rec.Body.String())
	}
	// alice cannot reach bob's subtree through the same policy.
	if rec := doAuth(t, h, "GET", "/v1/secret/data/users/userpass/bob/db", "", aliceTok); rec.Code != http.StatusForbidden {
		t.Fatalf("alice read of bob's subtree = %d, want 403", rec.Code)
	}
	// bob reaches his own, not alice's.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/users/userpass/bob/db", `{"data":{"k":"v"}}`, bobTok); rec.Code != http.StatusOK {
		t.Fatalf("bob write to own subtree = %d, want 200", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/secret/data/users/userpass/alice/db", "", bobTok); rec.Code != http.StatusForbidden {
		t.Fatalf("bob read of alice's subtree = %d, want 403", rec.Code)
	}
}

// TestTemplatedPolicyMetadataAndFailClosed confirms metadata templating works and
// that an unresolved placeholder grants nothing.
func TestTemplatedPolicyMetadataAndFailClosed(t *testing.T) {
	h, root := unsealedHandler(t)

	doAuth(t, h, "PUT", "/v1/sys/policies/acl/team-kv",
		`{"path":{"secret/data/teams/{{identity.entity.metadata.team}}/*":{"capabilities":["create","read","update"]}}}`, root)

	auth := makeUserAndLogin(t, h, root, "carol", "pw", `["team-kv"]`)
	tok := auth["client_token"].(string)
	entity := auth["entity_id"].(string)

	// No team metadata yet -> the templated rule drops -> no access.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/teams/platform/x", `{"data":{"k":"v"}}`, tok); rec.Code != http.StatusForbidden {
		t.Fatalf("pre-metadata write = %d, want 403 (rule drops)", rec.Code)
	}

	// Set team=platform on carol's entity.
	if rec := doAuth(t, h, "POST", "/v1/identity/entity",
		`{"id":"`+entity+`","policies":["team-kv"],"metadata":{"team":"platform"}}`, root); rec.Code != http.StatusOK {
		t.Fatalf("update entity = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Now the placeholder resolves and the same token reaches the team subtree.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/teams/platform/x", `{"data":{"k":"v"}}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("post-metadata write = %d, body=%s, want 200", rec.Code, rec.Body.String())
	}
	// But not another team's subtree.
	if rec := doAuth(t, h, "GET", "/v1/secret/data/teams/secops/x", "", tok); rec.Code != http.StatusForbidden {
		t.Fatalf("other-team read = %d, want 403", rec.Code)
	}
}
