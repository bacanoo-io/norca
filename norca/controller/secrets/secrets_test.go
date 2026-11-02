// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	issued  []*IssueRequest
	revoked []*RevokeRequest
	err     error
}

func (b *fakeBackend) Issue(_ context.Context, req *IssueRequest) (*Credential, error) {
	if b.err != nil {
		return nil, b.err
	}
	b.issued = append(b.issued, req)
	return &Credential{Items: map[string]string{"password": "s3cr3t"}, Ref: "user-1"}, nil
}

func (b *fakeBackend) Renew(_ context.Context, req *RenewRequest) (*Renewal, error) {
	if b.err != nil {
		return nil, b.err
	}
	return &Renewal{Items: map[string]string{"cert": req.Ref}}, nil
}

func (b *fakeBackend) Revoke(_ context.Context, req *RevokeRequest) error {
	b.revoked = append(b.revoked, req)
	return b.err
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// The JSON of the calls is the wire format of the servers
// (nomad/secretengine/operator.go): the names must not drift.
func TestHandler_WireFormat(t *testing.T) {
	b := &fakeBackend{}
	h := Handler(b)

	rec := post(t, h, PathIssue, `{"Namespace":"default","Engine":"pg","Role":"app",
		"Config":{"database":"app"},"Params":{"cn":"a"},"LeaseID":"l1","JobID":"j","TaskGroup":"g",
		"Task":"t","AllocID":"a","TTL":3600000000000,"MaxExpire":"2026-11-01T10:00:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("issue: %d", rec.Code)
	}
	req := b.issued[0]
	if req.TTL != time.Hour {
		t.Fatalf("ttl %s", req.TTL)
	}
	if req.Config["database"] != "app" || req.Params["cn"] != "a" || req.LeaseID != "l1" || req.Task != "t" {
		t.Fatalf("issue request: %+v", req)
	}
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Items", "Ref", "Expires"} {
		if _, ok := answer[key]; !ok {
			t.Fatalf("no %s in %s", key, rec.Body)
		}
	}

	rec = post(t, h, PathRevoke, `{"Namespace":"default","Engine":"pg","Revoked":{"user-1":"2026-11-01T10:00:00Z"}}`)
	if rec.Code != http.StatusOK || len(b.revoked[0].Revoked) != 1 {
		t.Fatalf("revoke: %d %+v", rec.Code, b.revoked)
	}

	rec = post(t, h, PathRenew, `{"LeaseID":"l1","Ref":"user-1","TTL":3600000000000}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cert":"user-1"`) {
		t.Fatalf("renew: %d %s", rec.Code, rec.Body)
	}
}

func TestHandler_Failures(t *testing.T) {
	b := &fakeBackend{err: errors.New("password=hunter2 refused")}
	h := Handler(b)

	rec := post(t, h, PathIssue, `{"LeaseID":"l1"}`)
	if rec.Code != http.StatusInternalServerError || rec.Body.Len() != 0 {
		t.Fatalf("a failure answers with a status only: %d %q", rec.Code, rec.Body)
	}
	if rec := post(t, h, PathIssue, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no lease ID: %d", rec.Code)
	}
	if rec := post(t, h, PathIssue, `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON: %d", rec.Code)
	}
	b.err = &Invalid{Reason: "common_name out of the role"}
	if rec := post(t, h, PathIssue, `{"LeaseID":"l1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid: %d", rec.Code)
	}
	if rec := post(t, h, PathHealth, ``); rec.Code != http.StatusOK {
		t.Fatalf("health: %d", rec.Code)
	}
}

// testCA issues certificates like the CA of a region.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) issue(t *testing.T, spiffe string) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(spiffe)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), URIs: []*url.URL{uri},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func writeWorkload(t *testing.T, ca *testCA) *TLS {
	t.Helper()
	dir := t.TempDir()
	der, key := ca.issue(t, "spiffe://global.norca/ns/default/job/op/group/op")
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certs := &TLS{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"),
		BundleFile: filepath.Join(dir, "bundle.pem")}
	for path, data := range map[string][]byte{
		certs.CertFile:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		certs.KeyFile:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		certs.BundleFile: ca.pem,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certs
}

// Only a certificate of the CA with the SPIFFE ID of the servers gets in.
func TestTLS_OnlyServers(t *testing.T) {
	ca := newTestCA(t)
	certs := writeWorkload(t, ca)
	config, err := certs.Config()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(Handler(&fakeBackend{}))
	srv.TLS = config
	srv.StartTLS()
	defer srv.Close()

	client := func(issuer *testCA, spiffe string) *http.Client {
		der, key := issuer.issue(t, spiffe)
		return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // the certificate names no address
			Certificates:       []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		}}}
	}
	do := func(c *http.Client) error {
		resp, err := c.Post(srv.URL+PathHealth, "application/json", bytes.NewReader(nil))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.New(resp.Status)
		}
		return nil
	}

	if err := do(client(ca, "spiffe://global.norca/norca/server")); err != nil {
		t.Fatalf("a server is refused: %v", err)
	}
	if err := do(client(ca, "spiffe://global.norca/ns/default/job/web/group/web")); err == nil {
		t.Fatal("a workload got in")
	}
	if err := do(client(newTestCA(t), "spiffe://global.norca/norca/server")); err == nil {
		t.Fatal("a certificate of another CA got in")
	}
}

func TestServe_Stops(t *testing.T) {
	certs := writeWorkload(t, newTestCA(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, "127.0.0.1:0", certs, &fakeBackend{}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
