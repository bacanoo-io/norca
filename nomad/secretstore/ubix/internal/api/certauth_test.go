package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func testClientCert(t *testing.T, cn string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

// certLoginReq builds a POST /v1/auth/cert/login request carrying cert as the
// mTLS client certificate.
func certLoginReq(cert *x509.Certificate) *http.Request {
	req := httptest.NewRequest("POST", "/v1/auth/cert/login", nil)
	if cert != nil {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	}
	return req
}

func TestCertAuthLoginOverHTTP(t *testing.T) {
	h, root := unsealedHandler(t)
	ca, caKey, caPEM := testCA(t)

	// Define a cert role trusting the CA, restricted to CN "alice".
	body, _ := json.Marshal(map[string]any{
		"certificate": caPEM, "policies": []string{"app-ro"}, "allowed_common_names": []string{"alice"},
	})
	if rec := doAuth(t, h, "POST", "/v1/auth/cert/certs/web", string(body), root); rec.Code != http.StatusNoContent {
		t.Fatalf("write cert role = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Login with a CA-signed cert for alice.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, certLoginReq(testClientCert(t, "alice", ca, caKey)))
	if rec.Code != http.StatusOK {
		t.Fatalf("cert login = %d, body=%s", rec.Code, rec.Body.String())
	}
	auth := decode[map[string]any](t, rec)["auth"].(map[string]any)
	if pols := auth["policies"].([]any); len(pols) != 1 || pols[0] != "app-ro" {
		t.Fatalf("policies = %v", pols)
	}

	// A cert from a different CA is rejected.
	otherCA, otherKey, _ := testCA(t)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, certLoginReq(testClientCert(t, "alice", otherCA, otherKey)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign cert login = %d, want 403", rec.Code)
	}

	// No certificate presented → 400.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, certLoginReq(nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-cert login = %d, want 400", rec.Code)
	}
}

func TestCertAuthConfigRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	_, _, caPEM := testCA(t)
	body, _ := json.Marshal(map[string]any{"certificate": caPEM, "policies": []string{"p"}})
	if rec := do(t, h, "POST", "/v1/auth/cert/certs/web", string(body)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("write cert role without token = %d, want 401", rec.Code)
	}
}
