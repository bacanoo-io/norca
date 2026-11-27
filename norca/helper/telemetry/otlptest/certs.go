// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package otlptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ServerName is a DNS name in the receiver's certificate besides localhost
// and 127.0.0.1.
const ServerName = "otlp.test"

// Certs is a test CA with a server and a client certificate, written as
// PEM files into a temporary directory.
type Certs struct {
	CAFile         string
	ServerCertFile string
	ServerKeyFile  string
	ClientCertFile string
	ClientKeyFile  string

	pool   *x509.CertPool
	server tls.Certificate
}

// NewCerts creates the CA and the certificates.
func NewCerts(t testing.TB) *Certs {
	t.Helper()
	dir := t.TempDir()
	c := &Certs{
		CAFile:         filepath.Join(dir, "ca.pem"),
		ServerCertFile: filepath.Join(dir, "server.pem"),
		ServerKeyFile:  filepath.Join(dir, "server-key.pem"),
		ClientCertFile: filepath.Join(dir, "client.pem"),
		ClientKeyFile:  filepath.Join(dir, "client-key.pem"),
	}

	caKey := newKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "otlptest CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("otlptest: CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("otlptest: CA: %v", err)
	}
	writePEM(t, c.CAFile, "CERTIFICATE", caDER)
	c.pool = x509.NewCertPool()
	c.pool.AddCert(caCert)

	issue := func(serial int64, name string, usage x509.ExtKeyUsage, certFile, keyFile string) tls.Certificate {
		key := newKey(t)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: name},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			DNSNames:     []string{ServerName, "localhost"},
			IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("otlptest: %s certificate: %v", name, err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("otlptest: %s key: %v", name, err)
		}
		writePEM(t, certFile, "CERTIFICATE", der)
		writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatalf("otlptest: %s key pair: %v", name, err)
		}
		return pair
	}
	c.server = issue(2, "otlptest server", x509.ExtKeyUsageServerAuth, c.ServerCertFile, c.ServerKeyFile)
	issue(3, "otlptest client", x509.ExtKeyUsageClientAuth, c.ClientCertFile, c.ClientKeyFile)
	return c
}

// ServerTLS is the receiver's TLS configuration; with requireClientCert it
// is mTLS against the test CA.
func (c *Certs) ServerTLS(requireClientCert bool) *tls.Config {
	tc := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{c.server},
	}
	if requireClientCert {
		tc.ClientAuth = tls.RequireAndVerifyClientCert
		tc.ClientCAs = c.pool
	}
	return tc
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("otlptest: key: %v", err)
	}
	return key
}

func writePEM(t testing.TB, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("otlptest: write %s: %v", path, err)
	}
}
