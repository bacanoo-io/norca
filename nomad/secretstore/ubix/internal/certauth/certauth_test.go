package certauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

func newMethod(t *testing.T) *Method {
	t.Helper()
	mem := storage.NewMemoryBackend()
	return New(mem, token.NewStore(mem), "auth/cert")
}

func certPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// makeCA returns a self-signed CA certificate (parsed + PEM) and its key.
func makeCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key, certPEM(der)
}

// makeClient issues a client cert for cn, signed by (ca, caKey). If caKey is nil
// the cert is self-signed. Returns the parsed cert and its PEM.
func makeClient(t *testing.T, cn string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) (*x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen client key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	parent, signer := ca, caKey
	if caKey == nil { // self-signed
		parent, signer = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, certPEM(der)
}

func TestLoginCASignedCert(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	ca, caKey, caPEM := makeCA(t)

	if err := m.WriteCert(ctx, "web", CertRole{
		Certificate:        caPEM,
		Policies:           []string{"readers"},
		AllowedCommonNames: []string{"alice"},
		TokenTTL:           time.Hour,
	}); err != nil {
		t.Fatalf("WriteCert: %v", err)
	}

	client, _ := makeClient(t, "alice", ca, caKey)
	tok, err := m.Login(ctx, []*x509.Certificate{client})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(tok.Policies) != 1 || tok.Policies[0] != "readers" {
		t.Fatalf("policies = %v", tok.Policies)
	}
}

func TestLoginSelfSignedPinnedCert(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)

	// Trust a specific self-signed client cert directly.
	client, clientPEM := makeClient(t, "svc", nil, nil)
	if err := m.WriteCert(ctx, "svc", CertRole{Certificate: clientPEM, Policies: []string{"p"}}); err != nil {
		t.Fatalf("WriteCert: %v", err)
	}
	if _, err := m.Login(ctx, []*x509.Certificate{client}); err != nil {
		t.Fatalf("Login with pinned self-signed cert: %v", err)
	}
}

func TestLoginRejections(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	ca, caKey, caPEM := makeCA(t)
	if err := m.WriteCert(ctx, "web", CertRole{
		Certificate:        caPEM,
		Policies:           []string{"p"},
		AllowedCommonNames: []string{"alice"},
	}); err != nil {
		t.Fatalf("WriteCert: %v", err)
	}

	// No certificate presented.
	if _, err := m.Login(ctx, nil); !errors.Is(err, ErrDenied) {
		t.Errorf("no cert: want ErrDenied, got %v", err)
	}
	// Certificate from a different CA.
	otherCA, otherKey, _ := makeCA(t)
	foreign, _ := makeClient(t, "alice", otherCA, otherKey)
	if _, err := m.Login(ctx, []*x509.Certificate{foreign}); !errors.Is(err, ErrDenied) {
		t.Errorf("foreign CA: want ErrDenied, got %v", err)
	}
	// Correct CA but a disallowed common name.
	bob, _ := makeClient(t, "bob", ca, caKey)
	if _, err := m.Login(ctx, []*x509.Certificate{bob}); !errors.Is(err, ErrDenied) {
		t.Errorf("disallowed CN: want ErrDenied, got %v", err)
	}
}

func TestWriteCertValidation(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t)
	_, _, caPEM := makeCA(t)

	if err := m.WriteCert(ctx, "x", CertRole{Certificate: caPEM}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("no policies: want ErrInvalidConfig, got %v", err)
	}
	if err := m.WriteCert(ctx, "x", CertRole{Certificate: "not a pem", Policies: []string{"p"}}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("bad PEM: want ErrInvalidConfig, got %v", err)
	}
	if err := m.WriteCert(ctx, "bad/name", CertRole{Certificate: caPEM, Policies: []string{"p"}}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("bad name: want ErrInvalidName, got %v", err)
	}
}
