package ldapauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// testCA is a private CA (like FreeIPA's Dogtag) and a server certificate it
// signed for 127.0.0.1.
type testCA struct {
	caPEM  string
	server tls.Certificate
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ldap.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{
		caPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		server: tls.Certificate{Certificate: [][]byte{srvDER}, PrivateKey: srvKey},
	}
}

// handshake dials a TLS server presenting ca's server certificate, using the TLS
// settings the LDAP method would use for cfg (URL pointed at the server).
func handshake(t *testing.T, ca testCA, cfg Config) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.server}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	cfg.URL = "ldaps://" + ln.Addr().String()
	tc, err := tlsConfig(&cfg)
	if err != nil {
		return err
	}
	conn, err := tls.Dial("tcp", ln.Addr().String(), tc)
	if err != nil {
		return err
	}
	return conn.Close()
}

func TestCertificateVerifiesPrivateCA(t *testing.T) {
	ca := newTestCA(t, "Dogtag test CA")

	if err := handshake(t, ca, Config{Certificate: ca.caPEM}); err != nil {
		t.Fatalf("with the directory's CA: %v", err)
	}
	// Without it the system roots cannot verify a private CA. (The wrapped cause
	// differs by platform — "unknown authority" on Linux, "not trusted" from the
	// macOS verifier — so assert on the verification failure itself.)
	var verr *tls.CertificateVerificationError
	if err := handshake(t, ca, Config{}); !errors.As(err, &verr) {
		t.Fatalf("without certificate: err = %v, want a certificate verification failure", err)
	}
	// ...and a different CA's bundle is not trusted in its place.
	other := newTestCA(t, "some other CA")
	if err := handshake(t, ca, Config{Certificate: other.caPEM}); !errors.As(err, &verr) {
		t.Fatalf("with another CA: err = %v, want a certificate verification failure", err)
	}
	// A bundle with the right CA among others works.
	if err := handshake(t, ca, Config{Certificate: other.caPEM + ca.caPEM}); err != nil {
		t.Fatalf("bundle containing the CA: %v", err)
	}
}

func TestCertPoolRejectsNonCertificates(t *testing.T) {
	ca := newTestCA(t, "CA")
	keyPEM := "-----BEGIN PRIVATE KEY-----\nMIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg\n-----END PRIVATE KEY-----\n"
	for name, bundle := range map[string]string{
		"empty":         "",
		"garbage":       "not a certificate",
		"private key":   keyPEM,
		"cert then key": ca.caPEM + keyPEM,
		"trailing junk": ca.caPEM + "junk",
		"bad DER":       "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	} {
		if _, err := certPool(bundle); !errors.Is(err, ErrInvalidCertificate) {
			t.Errorf("%s: err = %v, want ErrInvalidCertificate", name, err)
		}
	}
	if _, err := certPool(ca.caPEM); err != nil {
		t.Errorf("valid CA: %v", err)
	}
}

func TestConfigureCertificate(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	m := New(mem, token.NewStore(mem), "auth/ldap")
	ca := newTestCA(t, "CA")
	base := Config{URL: "ldaps://ldap.test", UserDN: "cn=users,dc=test"}

	withInsecure := base
	withInsecure.Certificate, withInsecure.InsecureTLS = ca.caPEM, true
	if err := m.Configure(ctx, withInsecure); !errors.Is(err, ErrCertificateWithInsecureTLS) {
		t.Fatalf("certificate + insecure_tls: err = %v", err)
	}
	bad := base
	bad.Certificate = "nope"
	if err := m.Configure(ctx, bad); !errors.Is(err, ErrInvalidCertificate) {
		t.Fatalf("bad certificate: err = %v", err)
	}
	good := base
	good.Certificate = ca.caPEM
	if err := m.Configure(ctx, good); err != nil {
		t.Fatal(err)
	}
	got, err := m.ReadConfig(ctx)
	if err != nil || !strings.Contains(got.Certificate, "BEGIN CERTIFICATE") {
		t.Fatalf("ReadConfig certificate = %q, err %v", got.Certificate, err)
	}
}
