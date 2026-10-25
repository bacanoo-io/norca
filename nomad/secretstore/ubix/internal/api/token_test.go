package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/token"
)

func TestTokenCreateWithTTL(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["p"],"ttl":"1h"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d, body=%s", rec.Code, rec.Body.String())
	}
	auth := decode[map[string]any](t, rec)["auth"].(map[string]any)
	if ld := auth["lease_duration"].(float64); ld < 3500 || ld > 3600 {
		t.Fatalf("lease_duration = %v, want ~3600", ld)
	}
}

func TestTokenCreateDefaultTTL(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["p"]}`, root)
	auth := decode[map[string]any](t, rec)["auth"].(map[string]any)
	if auth["lease_duration"].(float64) <= 0 {
		t.Fatalf("default lease_duration should be positive, got %v", auth["lease_duration"])
	}
}

// TestExpiredTokenRejected creates a token that expires essentially immediately
// (1ns) and confirms the middleware rejects it on the next request.
func TestExpiredTokenRejected(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["p"],"ttl":"1ns"}`, root)
	expired := decode[map[string]any](t, rec)["auth"].(map[string]any)["client_token"].(string)

	if rec := doAuth(t, h, "GET", "/v1/secret/data/anything", "", expired); rec.Code != http.StatusForbidden {
		t.Fatalf("expired token = %d, want 403", rec.Code)
	}
}

func TestRenewSelfExtendsLease(t *testing.T) {
	h, root := unsealedHandler(t)
	// A policy that lets a token renew itself.
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/renewer",
		`{"path":{"auth/token/renew-self":{"capabilities":["update"]}}}`, root)
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["renewer"],"ttl":"1h"}`, root)
	tok := decode[map[string]any](t, rec)["auth"].(map[string]any)["client_token"].(string)

	rec = doAuth(t, h, "POST", "/v1/auth/token/renew-self", `{"increment":"2h"}`, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ld := decode[map[string]any](t, rec)["auth"].(map[string]any)["lease_duration"].(float64); ld < 7100 || ld > 7200 {
		t.Fatalf("renewed lease_duration = %v, want ~7200", ld)
	}
}

// createPlainToken mints a 1h token whose only policy grants nothing, so any
// access it has comes from built-in rules rather than an ACL grant.
func createPlainToken(t *testing.T, h http.Handler, root string) string {
	t.Helper()
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["nothing"],"ttl":"1h"}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d, body=%s", rec.Code, rec.Body.String())
	}
	return decode[map[string]any](t, rec)["auth"].(map[string]any)["client_token"].(string)
}

func TestLookupSelfWithoutGrant(t *testing.T) {
	h, root := unsealedHandler(t)
	tok := createPlainToken(t, h, root)

	rec := doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	d := decode[map[string]any](t, rec)["data"].(map[string]any)
	if d["id"] != tok {
		t.Errorf("id = %v, want the calling token", d["id"])
	}
	if p := d["policies"].([]any); len(p) != 1 || p[0] != "nothing" {
		t.Errorf("policies = %v, want [nothing]", p)
	}
	if ttl := d["ttl"].(float64); ttl < 3500 || ttl > 3600 {
		t.Errorf("ttl = %v, want ~3600", ttl)
	}
	if d["expire_time"] == nil || d["renewable"] != true || d["type"] != "service" {
		t.Errorf("expire_time=%v renewable=%v type=%v", d["expire_time"], d["renewable"], d["type"])
	}
	if _, ok := d["identity_policies"].([]any); !ok {
		t.Errorf("identity_policies = %v, want a (possibly empty) list", d["identity_policies"])
	}
}

func TestLookupSelfRootToken(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	d := decode[map[string]any](t, rec)["data"].(map[string]any)
	if d["expire_time"] != nil || d["ttl"].(float64) != 0 || d["renewable"] != false {
		t.Errorf("root: expire_time=%v ttl=%v renewable=%v, want null/0/false", d["expire_time"], d["ttl"], d["renewable"])
	}
}

func TestLookupSelfRequiresToken(t *testing.T) {
	h, _ := unsealedHandler(t)
	if rec := doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
}

func TestRevokeSelfWithoutGrant(t *testing.T) {
	h, root := unsealedHandler(t)
	tok := createPlainToken(t, h, root)

	if rec := doAuth(t, h, "POST", "/v1/auth/token/revoke-self", "", tok); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", tok); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked token lookup-self = %d, want 403", rec.Code)
	}
}

// Every token may renew itself (as under Vault's default policy), but never past
// its ceiling: a 1h token asking for 10 years gets at most the system maximum.
func TestRenewSelfWithoutGrantIsCapped(t *testing.T) {
	h, root := unsealedHandler(t)
	tok := createPlainToken(t, h, root)
	rec := doAuth(t, h, "POST", "/v1/auth/token/renew-self", `{"increment":"87600h"}`, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	ld := decode[map[string]any](t, rec)["auth"].(map[string]any)["lease_duration"].(float64)
	if ceiling := token.DefaultMaxTTL.Seconds(); ld > ceiling {
		t.Fatalf("renewed lease_duration = %v s, want capped at %v s", ld, ceiling)
	}
}

// setPolicy writes an ACL policy as root.
func setPolicy(t *testing.T, h http.Handler, root, name, doc string) {
	t.Helper()
	if rec := doAuth(t, h, "PUT", "/v1/sys/policies/acl/"+name, doc, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write policy %s = %d, body=%s", name, rec.Code, rec.Body.String())
	}
}

func tokenWith(t *testing.T, h http.Handler, root, body string) string {
	t.Helper()
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", body, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d, body=%s", rec.Code, rec.Body.String())
	}
	return decode[map[string]any](t, rec)["auth"].(map[string]any)["client_token"].(string)
}

// The escalation this closes: a token allowed only to create tokens minted one
// carrying an admin policy (and read secrets it could not read itself).
func TestTokenCreateCannotEscalatePolicies(t *testing.T) {
	h, root := unsealedHandler(t)
	setPolicy(t, h, root, "admin", `{"path":{"*":{"capabilities":["create","read","update","delete","list"]}}}`)
	setPolicy(t, h, root, "minter", `{"path":{"auth/token/create":{"capabilities":["update"]}}}`)
	minter := tokenWith(t, h, root, `{"policies":["minter"],"ttl":"1h"}`)

	for _, body := range []string{
		`{"policies":["admin"]}`,
		`{"policies":["minter","admin"]}`,
		`{"policies":["root"]}`,
	} {
		rec := doAuth(t, h, "POST", "/v1/auth/token/create", body, minter)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("minter creating %s = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestTokenCreateSubsetAllowed(t *testing.T) {
	h, root := unsealedHandler(t)
	setPolicy(t, h, root, "minter", `{"path":{"auth/token/create":{"capabilities":["update"]}}}`)
	setPolicy(t, h, root, "reader", `{"path":{"secret/data/*":{"capabilities":["read"]}}}`)
	parent := tokenWith(t, h, root, `{"policies":["minter","reader"],"ttl":"1h"}`)

	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["reader"],"ttl":"30m"}`, parent)
	if rec.Code != http.StatusOK {
		t.Fatalf("subset create = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// A child cannot outlive its parent: a 1h parent asking for a 100-year child
// gets one bounded by the parent's own maximum lifetime.
func TestTokenCreateChildBoundedByParent(t *testing.T) {
	h, root := unsealedHandler(t)
	setPolicy(t, h, root, "minter", `{"path":{"auth/token/create":{"capabilities":["update"]}}}`)
	parent := tokenWith(t, h, root, `{"policies":["minter"],"ttl":"1h"}`)

	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["minter"],"ttl":"876000h"}`, parent)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d, body=%s", rec.Code, rec.Body.String())
	}
	ld := decode[map[string]any](t, rec)["auth"].(map[string]any)["lease_duration"].(float64)
	if ceiling := token.DefaultMaxTTL.Seconds(); ld > ceiling {
		t.Fatalf("child lease_duration = %v s, want at most the parent's ceiling (%v s)", ld, ceiling)
	}
}

// sudo on auth/token/create lifts both limits, as in Vault.
func TestTokenCreateWithSudo(t *testing.T) {
	h, root := unsealedHandler(t)
	setPolicy(t, h, root, "admin", `{"path":{"*":{"capabilities":["read"]}}}`)
	setPolicy(t, h, root, "issuer", `{"path":{"auth/token/create":{"capabilities":["update","sudo"]}}}`)
	issuer := tokenWith(t, h, root, `{"policies":["issuer"],"ttl":"1h"}`)

	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["admin"],"ttl":"8760h"}`, issuer)
	if rec.Code != http.StatusOK {
		t.Fatalf("sudo create = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ld := decode[map[string]any](t, rec)["auth"].(map[string]any)["lease_duration"].(float64); ld < 8759*3600 {
		t.Fatalf("sudo child lease_duration = %v s, want the full year", ld)
	}
}

// Root keeps issuing long-lived tokens (the CI-token workflow): 8760h stays 8760h.
func TestRootCanStillIssueLongLivedTokens(t *testing.T) {
	h, root := unsealedHandler(t)
	rec := doAuth(t, h, "POST", "/v1/auth/token/create", `{"policies":["ci-ro"],"ttl":"8760h"}`, root)
	if ld := decode[map[string]any](t, rec)["auth"].(map[string]any)["lease_duration"].(float64); ld < 8759*3600 {
		t.Fatalf("root-issued 8760h token lease_duration = %v s", ld)
	}
}

// The sweeper removes expired tokens and what they owned (their cubbyhole),
// and leaves live tokens alone.
func TestSweepExpiredTokensDestroysCubbyhole(t *testing.T) {
	h, root := unsealedHandler(t)
	short := tokenWith(t, h, root, `{"policies":["p"],"ttl":"2s"}`)
	live := tokenWith(t, h, root, `{"policies":["p"],"ttl":"1h"}`)
	for _, tok := range []string{short, live} {
		if rec := doAuth(t, h, "POST", "/v1/cubbyhole/note", `{"k":"v"}`, tok); rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
			t.Fatalf("cubbyhole write = %d, body=%s", rec.Code, rec.Body.String())
		}
	}
	time.Sleep(2100 * time.Millisecond)

	n, err := h.(*Handler).sweepExpiredTokens(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	if rec := doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", live); rec.Code != http.StatusOK {
		t.Fatalf("live token after sweep = %d", rec.Code)
	}
	// The expired token's cubbyhole is gone from storage, not just unreachable.
	keys, err := h.(*Handler).core.Barrier().List(context.Background(), cubbyholeMountPrefix+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("cubbyhole scopes after sweep = %v, want only the live token's", keys)
	}
}

// A delegated child (created by a non-root, non-sudo token) is revoked with its
// parent, cubbyhole and all; lookup-self reports orphan accordingly.
func TestRevokeSelfCascadesToDelegatedChildren(t *testing.T) {
	h, root := unsealedHandler(t)
	setPolicy(t, h, root, "minter", `{"path":{"auth/token/create":{"capabilities":["update"]}}}`)
	parent := tokenWith(t, h, root, `{"policies":["minter"],"ttl":"1h"}`)
	child := tokenWith(t, h, parent, `{"policies":["minter"],"ttl":"30m"}`)
	grandchild := tokenWith(t, h, child, `{"policies":["minter"],"ttl":"10m"}`)
	doAuth(t, h, "POST", "/v1/cubbyhole/note", `{"k":"v"}`, grandchild)

	orphan := func(tok string) any {
		return decode[map[string]any](t, doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", tok))["data"].(map[string]any)["orphan"]
	}
	if orphan(parent) != true || orphan(child) != false {
		t.Fatalf("orphan: parent=%v child=%v, want true/false", orphan(parent), orphan(child))
	}

	if rec := doAuth(t, h, "POST", "/v1/auth/token/revoke-self", "", parent); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke-self = %d, body=%s", rec.Code, rec.Body.String())
	}
	for name, tok := range map[string]string{"child": child, "grandchild": grandchild} {
		if rec := doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", tok); rec.Code != http.StatusForbidden {
			t.Errorf("%s after parent revoked = %d, want 403", name, rec.Code)
		}
	}
	if keys, _ := h.(*Handler).core.Barrier().List(context.Background(), cubbyholeMountPrefix+"/"); len(keys) != 0 {
		t.Errorf("grandchild's cubbyhole survived the cascade: %v", keys)
	}
}

// Tokens created by a sudo holder are orphans: revoking the issuer does not
// take down what it issued (the operator workflow, e.g. long-lived CI tokens).
func TestSudoCreatedTokensSurviveIssuerRevocation(t *testing.T) {
	h, root := unsealedHandler(t)
	setPolicy(t, h, root, "issuer", `{"path":{"auth/token/create":{"capabilities":["update","sudo"]}}}`)
	issuer := tokenWith(t, h, root, `{"policies":["issuer"],"ttl":"1h"}`)
	issued := tokenWith(t, h, issuer, `{"policies":["ci-ro"],"ttl":"8760h"}`)

	doAuth(t, h, "POST", "/v1/auth/token/revoke-self", "", issuer)
	if rec := doAuth(t, h, "GET", "/v1/auth/token/lookup-self", "", issued); rec.Code != http.StatusOK {
		t.Fatalf("sudo-issued token after issuer revoked = %d, want 200", rec.Code)
	}
}

// A userpass user's token_max_ttl bounds the tokens it logs in with: a 10-year
// renewal stops at the role's ceiling. The value round-trips on read.
func TestRoleTokenMaxTTLBoundsLoginTokens(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/auth/userpass/users/ci", `{"password":"pw-ci-test","policies":["p"],"token_ttl":"30m","token_max_ttl":"1h"}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write user = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec := doAuth(t, h, "GET", "/v1/auth/userpass/users/ci", "", root)
	if got := decode[map[string]any](t, rec)["data"].(map[string]any)["token_max_ttl"]; got != "1h0m0s" {
		t.Fatalf("read token_max_ttl = %v, want 1h0m0s", got)
	}
	login := doAuth(t, h, "POST", "/v1/auth/userpass/login/ci", `{"password":"pw-ci-test"}`, "")
	tok := decode[map[string]any](t, login)["auth"].(map[string]any)["client_token"].(string)
	renew := doAuth(t, h, "POST", "/v1/auth/token/renew-self", `{"increment":"87600h"}`, tok)
	if ld := decode[map[string]any](t, renew)["auth"].(map[string]any)["lease_duration"].(float64); ld > 3600 {
		t.Fatalf("renewed lease_duration = %v s, want at most the role's 1h", ld)
	}
}

// token_ttl above token_max_ttl is refused, on every auth method that takes them.
func TestRoleTokenTTLAboveMaxIsRefused(t *testing.T) {
	h, root := unsealedHandler(t)
	for path, body := range map[string]string{
		"/v1/auth/userpass/users/u":  `{"password":"pw","policies":["p"],"token_ttl":"2h","token_max_ttl":"1h"}`,
		"/v1/auth/approle/role/r":    `{"policies":["p"],"token_ttl":"2h","token_max_ttl":"1h"}`,
		"/v1/auth/kubernetes/role/k": `{"bound_service_account_names":["a"],"bound_service_account_namespaces":["n"],"policies":["p"],"ttl":"2h","token_max_ttl":"1h"}`,
		"/v1/auth/cert/certs/c":      `{"policies":["p"],"certificate":"x","token_ttl":"2h","token_max_ttl":"1h"}`,
		"/v1/auth/jwt/role/j":        `{"policies":["p"],"bound_audiences":["a"],"token_ttl":"2h","token_max_ttl":"1h"}`,
	} {
		rec := doAuth(t, h, "POST", path, body, root)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "token_max_ttl") {
			t.Errorf("%s: %d %s, want 400 about token_max_ttl", path, rec.Code, rec.Body.String())
		}
	}
}
