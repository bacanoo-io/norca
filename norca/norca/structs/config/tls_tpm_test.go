// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/tpmkey"
	"github.com/bacanoo-io/norca/norca/helper/tpmkey/tpmtest"
)

// The key loader loads a key of the TPM (a TSS2 key file) as the key of
// the certificate of the agent: TLS handshakes sign with the TPM, and a
// reload (SIGHUP) after a rotation loads the new key.
func TestKeyLoader_TPMKey(t *testing.T) {
	open := tpmtest.Start(t)
	prev := openTPM
	openTPM = open
	t.Cleanup(func() { openTPM = prev })

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	ca, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "client.pem"), filepath.Join(dir, "client-key.pem")
	// newPair makes a key in the TPM and writes it with its certificate,
	// as norca-join does.
	newPair := func(serial int64) crypto.PublicKey {
		t.Helper()
		s, keyPEM, err := tpmkey.Generate(open)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "node"},
			DNSNames: []string{"client.global.norca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, s.Public(), caKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		return s.Public()
	}

	kl := &KeyLoader{}
	pub1 := newPair(2)
	cert, err := kl.LoadKeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cert.PrivateKey.(*tpmkey.Signer); !ok || !pub1.(*ecdsa.PublicKey).Equal(cert.PrivateKey.(crypto.Signer).Public()) {
		t.Fatalf("the private key is not the TPM key: %T", cert.PrivateKey)
	}
	handshake(t, kl, pool)

	// A renewal: a new key in the TPM, loaded again (SIGHUP).
	_ = cert.PrivateKey.(*tpmkey.Signer).Close()
	pub2 := newPair(3)
	cert, err = kl.LoadKeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if pub := cert.PrivateKey.(crypto.Signer).Public(); !pub2.(*ecdsa.PublicKey).Equal(pub) || pub1.(*ecdsa.PublicKey).Equal(pub) {
		t.Fatal("the reload kept the old key")
	}
	handshake(t, kl, pool)
	_ = cert.PrivateKey.(*tpmkey.Signer).Close()

	// A private blob the TPM does not take (of another TPM, or broken as
	// here) fails at the load, not at the first handshake.
	keyPEM, _ := os.ReadFile(keyFile)
	block, _ := pem.Decode(keyPEM)
	block.Bytes[len(block.Bytes)-10] ^= 0xff // the private blob is the last field
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := kl.LoadKeyPair(certFile, keyFile); err == nil || !strings.Contains(err.Error(), "TPM") {
		t.Fatalf("a broken TPM key file was loaded: %v", err)
	}
}

// handshake runs a mutual TLS handshake with the certificate of the key
// loader on both sides, as the RPC of the agents does.
func handshake(t *testing.T, kl *KeyLoader, pool *x509.CertPool) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		GetCertificate: kl.GetOutgoingCertificate, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	start := time.Now()
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 30 * time.Second}, Config: &tls.Config{
		RootCAs: pool, ServerName: "client.global.norca", GetClientCertificate: kl.GetClientCertificate,
		MinVersion: tls.VersionTLS12,
	}}
	c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	t.Logf("a mutual handshake with the TPM key on both sides: %s", time.Since(start))
}
