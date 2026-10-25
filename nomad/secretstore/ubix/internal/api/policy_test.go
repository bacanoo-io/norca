package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// createToken mints a token with the given policies using the root token and
// returns its id.
func createToken(t *testing.T, h http.Handler, root, policiesJSON string) string {
	t.Helper()
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":`+policiesJSON+`}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("token create = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decode[map[string]any](t, rec)
	return body["auth"].(map[string]any)["client_token"].(string)
}

func TestPolicyCRUD(t *testing.T) {
	h, root := unsealedHandler(t)

	// Write a policy.
	doc := `{"path":{"secret/data/app/*":{"capabilities":["read","list"]}}}`
	if rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/readers", doc, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write policy = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Read it back.
	rec := doAuth(t, h, "GET", "/v1/sys/policies/acl/readers", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("read policy = %d", rec.Code)
	}
	if decode[map[string]any](t, rec)["data"].(map[string]any)["name"] != "readers" {
		t.Fatalf("policy read body = %s", rec.Body.String())
	}

	// List includes it.
	rec = doAuth(t, h, "LIST", "/v1/sys/policies/acl", "", root)
	keys := decode[map[string]any](t, rec)["data"].(map[string]any)["keys"].([]any)
	if len(keys) != 1 || keys[0] != "readers" {
		t.Fatalf("list = %v", keys)
	}

	// Delete it.
	if rec := doAuth(t, h, "DELETE", "/v1/sys/policies/acl/readers", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("delete policy = %d", rec.Code)
	}
	if rec := doAuth(t, h, "GET", "/v1/sys/policies/acl/readers", "", root); rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete = %d, want 404", rec.Code)
	}
}

func TestPolicyWriteInvalidCapability(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/bad",
		`{"path":{"secret/*":{"capabilities":["frobnicate"]}}}`, root)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid capability = %d, want 400", rec.Code)
	}
}

// TestScopedTokenEnforcement is the core authorization test: a token with a
// read-only policy on secret/data/app/* can read those secrets but not write
// them, and cannot touch other paths.
func TestScopedTokenEnforcement(t *testing.T) {
	h, root := unsealedHandler(t)

	// Root seeds a secret and a read-only policy.
	doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"pw":"s3cr3t"}}`, root)
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/app-ro",
		`{"path":{"secret/data/app/*":{"capabilities":["read"]}}}`, root)

	// Mint a scoped token.
	scoped := createToken(t, h, root, `["app-ro"]`)

	// It CAN read the app secret.
	if rec := doAuth(t, h, "GET", "/v1/secret/data/app/db", "", scoped); rec.Code != http.StatusOK {
		t.Fatalf("scoped read = %d, want 200", rec.Code)
	}
	// It CANNOT write the app secret.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"pw":"x"}}`, scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped write = %d, want 403", rec.Code)
	}
	// It CANNOT read a different path.
	if rec := doAuth(t, h, "GET", "/v1/secret/data/other", "", scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped cross-path read = %d, want 403", rec.Code)
	}
	// It CANNOT manage policies.
	if rec := doAuth(t, h, "LIST", "/v1/sys/policies/acl", "", scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("scoped policy list = %d, want 403", rec.Code)
	}
}

func TestScopedTokenCannotCreateTokens(t *testing.T) {
	h, root := unsealedHandler(t)
	scoped := createToken(t, h, root, `["nonexistent-policy"]`) // no grants at all

	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["x"]}`, scoped)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped token create = %d, want 403", rec.Code)
	}
}

// TestPolicyAcceptsHCL confirms a HashiCorp-style HCL policy is accepted and
// enforced identically to the JSON form.
func TestPolicyAcceptsHCL(t *testing.T) {
	h, root := unsealedHandler(t)

	doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"pw":"s3cr3t"}}`, root)
	hcl := "path \"secret/data/app/*\" {\n  capabilities = [\"read\"]\n}\n"
	if rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/app-ro", hcl, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write HCL policy = %d, body=%s", rec.Code, rec.Body.String())
	}

	scoped := createToken(t, h, root, `["app-ro"]`)
	if rec := doAuth(t, h, "GET", "/v1/secret/data/app/db", "", scoped); rec.Code != http.StatusOK {
		t.Fatalf("HCL-policy read = %d, want 200", rec.Code)
	}
	if rec := doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"pw":"x"}}`, scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("HCL-policy write = %d, want 403", rec.Code)
	}
}

func TestPolicyRejectsMalformedHCL(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/bad", `path "a" { capabilities = [`, root)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed HCL = %d, want 400", rec.Code)
	}
}

// Vault's clients (and Terraform) write {"policy": "<text>"}; before, that was
// taken as the document itself and stored as an EMPTY policy with a 204.
func TestPolicyWriteVaultWrappedBody(t *testing.T) {
	h, root := unsealedHandler(t)
	body := `{"policy":"path \"secret/data/*\" { capabilities = [\"read\"] }"}`
	if rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/reader", body, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write = %d, body=%s", rec.Code, rec.Body.String())
	}
	doAuth(t, h, "POST", "/v1/secret/data/app", `{"data":{"k":"v"}}`, root)
	tok := tokenWith(t, h, root, `{"policies":["reader"],"ttl":"1h"}`)
	if rec := doAuth(t, h, "GET", "/v1/secret/data/app", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("token with the written policy reading secret = %d, want 200 (policy stored empty?)", rec.Code)
	}
}

// Reads return the policy as a string, as Vault does, and that string can be
// written back unchanged.
func TestPolicyReadIsStringAndRoundTrips(t *testing.T) {
	h, root := unsealedHandler(t)
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/p", `path "secret/data/*" { capabilities = ["read", "list"] }`, root)
	rec := doAuth(t, h, "GET", "/v1/sys/policies/acl/p", "", root)
	text, ok := decode[map[string]any](t, rec)["data"].(map[string]any)["policy"].(string)
	if !ok || !strings.Contains(text, "secret/data/*") {
		t.Fatalf("policy = %#v, want a string containing the path", decode[map[string]any](t, rec)["data"])
	}
	wrapped, _ := json.Marshal(map[string]string{"policy": text})
	if rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/p2", string(wrapped), root); rec.Code != http.StatusNoContent {
		t.Fatalf("writing the read-back text = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// Unknown keys must fail loudly: a typo would otherwise store an empty policy,
// and a dropped restriction (allowed_parameters) would grant more than written.
func TestPolicyWriteRejectsUnknownKeys(t *testing.T) {
	h, root := unsealedHandler(t)
	for name, body := range map[string]string{
		"misspelled path":         `{"paths":{"secret/*":{"capabilities":["read"]}}}`,
		"allowed_parameters":      `{"path":{"secret/*":{"capabilities":["update"],"allowed_parameters":{"k":[]}}}}`,
		"wrapped with extra keys": `{"policy":"path \"a\" { capabilities = [\"read\"] }","name":"x"}`,
		"wrapped non-string":      `{"policy":{"path":{}}}`,
	} {
		if rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/bad", body, root); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: write = %d, want 400; body=%s", name, rec.Code, rec.Body.String())
		}
	}
	// The documented bare-document forms keep working.
	if rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/ok", `{"path":{"secret/*":{"capabilities":["read"]}}}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("bare JSON document = %d", rec.Code)
	}
}
