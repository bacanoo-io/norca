package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/ldapauth"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// fakeLDAP is an in-memory ldapauth.Connector: it accepts one password and
// returns fixed groups, so the HTTP layer is testable without a directory.
type fakeLDAP struct {
	password string
	groups   []string
}

func (f fakeLDAP) Authenticate(_ context.Context, _ *ldapauth.Config, _ string, password string) ([]string, error) {
	if password != f.password {
		return nil, ldapauth.ErrDenied
	}
	return f.groups, nil
}

// unsealedLDAPHandler returns an unsealed handler whose LDAP method uses fake,
// plus the root token.
func unsealedLDAPHandler(t *testing.T, fake ldapauth.Connector) (*Handler, string) {
	t.Helper()
	c := core.New(storage.NewMemoryBackend())
	h := NewHandler(c)
	h.ldap = ldapauth.NewWithConnector(c.Barrier(), c.Tokens(), "auth/ldap", fake)

	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[1]+`"}`)
	return h, init.RootToken
}

func TestLDAPLoginFlow(t *testing.T) {
	h, root := unsealedLDAPHandler(t, fakeLDAP{password: "correct", groups: []string{"platform"}})

	// Configure the directory and map the "platform" LDAP group to a policy.
	if rec := doAuth(t, h, "POST", "/v1/auth/ldap/config",
		`{"url":"ldap://dir.test:389","user_dn":"ou=people,dc=test","group_dn":"ou=groups,dc=test","bind_dn":"cn=svc,dc=test","bind_password":"s3cret"}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("configure = %d, body=%s", rec.Code, rec.Body.String())
	}
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/kv-reader",
		`{"path":{"secret/data/app":{"capabilities":["read","create","update"]}}}`, root)
	if rec := doAuth(t, h, "POST", "/v1/auth/ldap/groups/platform", `{"policies":["kv-reader"]}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("write group = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Login with the right password → token carrying the group's policy.
	rec := do(t, h, "POST", "/v1/auth/ldap/login/alice", `{"password":"correct"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, body=%s", rec.Code, rec.Body.String())
	}
	auth := decode[map[string]any](t, rec)["auth"].(map[string]any)
	tok := auth["client_token"].(string)
	if pols := auth["policies"].([]any); len(pols) != 1 || pols[0] != "kv-reader" {
		t.Fatalf("policies = %v, want [kv-reader]", pols)
	}
	// The token has an identity entity (login went through the aliaser).
	if auth["entity_id"].(string) == "" {
		t.Fatal("ldap login produced no entity_id")
	}

	// And the policy actually works.
	if rec := doAuth(t, h, "POST", "/v1/secret/data/app", `{"data":{"k":"v"}}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("token write = %d, body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestLDAPLoginBadPassword(t *testing.T) {
	h, root := unsealedLDAPHandler(t, fakeLDAP{password: "correct"})
	doAuth(t, h, "POST", "/v1/auth/ldap/config",
		`{"url":"ldap://dir.test","user_dn":"ou=people,dc=test"}`, root)

	if rec := do(t, h, "POST", "/v1/auth/ldap/login/alice", `{"password":"wrong"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("bad password login = %d, want 403", rec.Code)
	}
}

func TestLDAPReadConfigRedactsPassword(t *testing.T) {
	h, root := unsealedLDAPHandler(t, fakeLDAP{})
	doAuth(t, h, "POST", "/v1/auth/ldap/config",
		`{"url":"ldap://dir.test","user_dn":"ou=people,dc=test","bind_dn":"cn=svc","bind_password":"s3cret"}`, root)

	rec := doAuth(t, h, "GET", "/v1/auth/ldap/config", "", root)
	if rec.Code != http.StatusOK {
		t.Fatalf("read config = %d", rec.Code)
	}
	data := decode[map[string]any](t, rec)["data"].(map[string]any)
	if _, present := data["bind_password"]; present {
		t.Fatal("read config leaked bind_password")
	}
	if data["bind_password_set"] != true {
		t.Fatalf("bind_password_set = %v, want true", data["bind_password_set"])
	}
}

func TestLDAPConfigRequiresAuth(t *testing.T) {
	h, _ := unsealedLDAPHandler(t, fakeLDAP{})
	if rec := do(t, h, "POST", "/v1/auth/ldap/config", `{"url":"ldap://x","user_dn":"y"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("config without token = %d, want 401", rec.Code)
	}
}

// generatedCAPEM returns a freshly generated self-signed CA certificate (PEM).
// Content is irrelevant beyond parsing; TLS behavior is tested in ldapauth.
func generatedCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "LDAP test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestLDAPConfigCertificate(t *testing.T) {
	h, root := unsealedHandler(t)
	ca := generatedCAPEM(t)
	body := func(extra string) string {
		return `{"url":"ldaps://ldap.test","user_dn":"cn=users,dc=test"` + extra + `}`
	}
	if rec := doAuth(t, h, "POST", "/v1/auth/ldap/config", body(`,"certificate":`+jsonString(ca)), root); rec.Code != http.StatusNoContent {
		t.Fatalf("config with certificate = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec := doAuth(t, h, "GET", "/v1/auth/ldap/config", "", root)
	if got := decode[map[string]any](t, rec)["data"].(map[string]any)["certificate"]; got != ca {
		t.Fatalf("read back certificate = %v", got)
	}
	for name, extra := range map[string]string{
		"private key":   `,"certificate":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"`,
		"not PEM":       `,"certificate":"hello"`,
		"with insecure": `,"certificate":` + jsonString(ca) + `,"insecure_tls":true`,
	} {
		if rec := doAuth(t, h, "POST", "/v1/auth/ldap/config", body(extra), root); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: config = %d, want 400; body=%s", name, rec.Code, rec.Body.String())
		}
	}
}
