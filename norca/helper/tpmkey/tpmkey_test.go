// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tpmkey_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/tpmkey"
	"github.com/bacanoo-io/norca/norca/helper/tpmkey/tpmtest"
)

func TestGenerateAndLoad(t *testing.T) {
	open := tpmtest.Start(t)
	s, keyPEM, err := tpmkey.Generate(open)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.HasPrefix(string(keyPEM), "-----BEGIN TSS2 PRIVATE KEY-----") || !tpmkey.IsPEM(keyPEM) {
		t.Fatalf("not a TSS2 PEM key file:\n%s", keyPEM)
	}
	pub, ok := s.Public().(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("not an ECDSA P-256 key: %T", s.Public())
	}
	digest := sha256.Sum256([]byte("node"))
	sig, err := s.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Fatal("the signature does not verify")
	}

	// The key file read again signs with the same key.
	s2, err := tpmkey.NewSigner(keyPEM, open)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Check(); err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(s2.Public()) {
		t.Fatal("the key file has another public key")
	}
	_ = s.Close() // one key loaded at a time: swtpm has three object slots
	sig, err = s2.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil || !ecdsa.VerifyASN1(pub, digest[:], sig) {
		t.Fatalf("the reloaded key: %v", err)
	}
	// A closed Signer opens the TPM again.
	_ = s2.Close()
	if _, err := s2.Sign(rand.Reader, digest[:], crypto.SHA256); err != nil {
		t.Fatalf("after Close: %v", err)
	}
}

func TestCertificateRequest(t *testing.T) {
	open := tpmtest.Start(t)
	s, _, err := tpmkey.Generate(open)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "a-1-1"}}, s)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("the signature of the request: %v", err)
	}
}

func TestRotationMakesNewKeys(t *testing.T) {
	open := tpmtest.Start(t)
	s1, pem1, err := tpmkey.Generate(open)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()
	s2, pem2, err := tpmkey.Generate(open)
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	if s1.Public().(*ecdsa.PublicKey).Equal(s2.Public()) || string(pem1) == string(pem2) {
		t.Fatal("two keys are the same")
	}
}

func TestNotTPMKey(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(k)
	plain := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if tpmkey.IsPEM(plain) || tpmkey.IsPEM([]byte("junk")) {
		t.Fatal("a plain key is a TPM key")
	}
	if _, err := tpmkey.NewSigner(plain, nil); err == nil {
		t.Fatal("a plain key gave a Signer")
	}
}

// TestTLSHandshake runs TLS 1.2 and 1.3 handshakes in which the server and
// the client both sign with keys of the TPM, concurrently.
func TestTLSHandshake(t *testing.T) {
	open := tpmtest.Start(t)
	ca := newCA(t)
	srvSigner, srvKey, err := tpmkey.Generate(open)
	if err != nil {
		t.Fatal(err)
	}
	cliSigner, cliKey, err := tpmkey.Generate(open)
	if err != nil {
		t.Fatal(err)
	}
	srvCert := loadPair(t, ca.issue(t, srvSigner.Public(), "server.global.norca", x509.ExtKeyUsageServerAuth), srvKey, open)
	cliCert := loadPair(t, ca.issue(t, cliSigner.Public(), "client.global.norca", x509.ExtKeyUsageClientAuth), cliKey, open)
	_ = srvSigner.Close()
	_ = cliSigner.Close()
	defer srvCert.PrivateKey.(*tpmkey.Signer).Close()
	defer cliCert.PrivateKey.(*tpmkey.Signer).Close()

	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
			Certificates: []tls.Certificate{srvCert}, ClientCAs: ca.pool, ClientAuth: tls.RequireAndVerifyClientCert,
			MinVersion: version, MaxVersion: version,
		})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					_, _ = io.Copy(c, c)
				}()
			}
		}()
		const n = 4
		var wg sync.WaitGroup
		errs := make(chan error, n)
		start := time.Now()
		for range n {
			wg.Go(func() {
				d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 30 * time.Second}, Config: &tls.Config{
					RootCAs: ca.pool, ServerName: "server.global.norca", Certificates: []tls.Certificate{cliCert},
					MinVersion: version, MaxVersion: version,
				}}
				c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
				if err != nil {
					errs <- err
					return
				}
				defer c.Close()
				if _, err := c.Write([]byte("ping")); err != nil {
					errs <- err
					return
				}
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("TLS %x: %v", version, err)
		}
		t.Logf("TLS %x: %d mutual handshakes with TPM keys in %s", version, n, time.Since(start))
		_ = ln.Close()
	}
}

func TestX509KeyPairRefusesOtherKey(t *testing.T) {
	open := tpmtest.Start(t)
	ca := newCA(t)
	s, keyPEM, err := tpmkey.Generate(open)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	certPEM := ca.issue(t, other.Public(), "client.global.norca", x509.ExtKeyUsageClientAuth)
	if _, err := tpmkey.X509KeyPair(certPEM, keyPEM, open); err == nil {
		t.Fatal("a certificate of another key was accepted")
	}
}

func loadPair(t *testing.T, certPEM, keyPEM []byte, open tpmkey.Opener) tls.Certificate {
	t.Helper()
	pair, err := tpmkey.X509KeyPair(certPEM, keyPEM, open)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func (ca *testCA) issue(t *testing.T, pub crypto.PublicKey, name string, usage x509.ExtKeyUsage) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
