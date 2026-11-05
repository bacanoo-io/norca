// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tlsutil

import (
	"crypto"
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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
)

// testPKI is a cluster CA, the server certificate it signs, and node CAs
// (intermediates) with the client certificates they sign.
type testPKI struct {
	t      *testing.T
	dir    string
	ca     *x509.Certificate
	caKey  crypto.Signer
	serial int64
}

func newTestPKI(t *testing.T) *testPKI {
	p := &testPKI{t: t, dir: t.TempDir()}
	p.caKey = p.key()
	p.ca = p.sign(&x509.Certificate{
		Subject: pkix.Name{CommonName: "cluster CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}, p.caKey.Public(), nil, p.caKey)
	return p
}

func (p *testPKI) key() crypto.Signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(p.t, err)
	return k
}

// sign signs the template with the parent's key (the template itself when
// parent is nil).
func (p *testPKI) sign(tmpl *x509.Certificate, pub crypto.PublicKey, parent *x509.Certificate, parentKey crypto.Signer) *x509.Certificate {
	p.serial++
	tmpl.SerialNumber = big.NewInt(p.serial)
	tmpl.NotBefore = time.Now().Add(-time.Minute)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, parentKey)
	must.NoError(p.t, err)
	cert, err := x509.ParseCertificate(der)
	must.NoError(p.t, err)
	return cert
}

// nodeCA issues an intermediate with the key.
func (p *testPKI) nodeCA(name string, key crypto.Signer) *x509.Certificate {
	return p.sign(&x509.Certificate{
		Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign,
	}, key.Public(), p.ca, p.caKey)
}

// leaf issues a certificate for the name, signed by the parent.
func (p *testPKI) leaf(name string, parent *x509.Certificate, parentKey crypto.Signer) (*x509.Certificate, crypto.Signer) {
	key := p.key()
	cert := p.sign(&x509.Certificate{
		Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}, key.Public(), parent, parentKey)
	return cert, key
}

// write writes the certificates (and the key) as PEM files and returns the
// path of the first.
func (p *testPKI) write(name string, key crypto.Signer, certs ...*x509.Certificate) string {
	var data []byte
	for _, c := range certs {
		data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	path := filepath.Join(p.dir, name+".pem")
	must.NoError(p.t, os.WriteFile(path, data, 0o600))
	if key != nil {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		must.NoError(p.t, err)
		must.NoError(p.t, os.WriteFile(filepath.Join(p.dir, name+"-key.pem"),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	}
	return path
}

// server starts a TLS listener with the incoming configuration of a server
// that verifies the clients, and returns its address.
func (p *testPKI) server(distrusted string) (string, error) {
	cert, key := p.leaf("server.global.norca", p.ca, p.caKey)
	certFile := p.write("server", key, cert)
	conf := &config.TLSConfig{
		EnableRPC: true, CAFile: p.write("ca", nil, p.ca),
		CertFile: certFile, KeyFile: filepath.Join(p.dir, "server-key.pem"),
		DistrustedCAFile: distrusted,
	}
	c, err := NewTLSConfiguration(conf, true, true)
	must.NoError(p.t, err)
	tlsConf, err := c.IncomingTLSConfig()
	if err != nil {
		return "", err
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsConf)
	must.NoError(p.t, err)
	p.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.(*tls.Conn).Handshake()
				buf := make([]byte, 1)
				_, _ = conn.Read(buf)
			}()
		}
	}()
	return ln.Addr().String(), nil
}

// dial connects with the client certificate (leaf, then the intermediate)
// and reports whether the server accepted it.
func (p *testPKI) dial(addr string, leaf *x509.Certificate, key crypto.Signer, chain ...*x509.Certificate) error {
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	raw := [][]byte{leaf.Raw}
	for _, c := range chain {
		raw = append(raw, c.Raw)
	}
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		RootCAs: pool, ServerName: "server.global.norca", MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: raw, PrivateKey: key}},
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	// With TLS 1.3 the server's verdict on the client certificate arrives
	// after the handshake: a write and a read bring it.
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{1}); err != nil {
		return err
	}
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err != nil && err.Error() == "EOF" {
		return nil // the server took the byte and closed: accepted
	}
	return err
}

func TestDistrustedCA(t *testing.T) {
	ci.Parallel(t)
	p := newTestPKI(t)

	stolenKey := p.key()
	stolen := p.nodeCA("node CA 1", stolenKey)
	goodKey := p.key()
	good := p.nodeCA("node CA 2", goodKey)
	leafStolen, keyStolen := p.leaf("client.global.norca", stolen, stolenKey)
	leafGood, keyGood := p.leaf("client.global.norca", good, goodKey)

	// Nothing distrusted: both node CAs are accepted.
	addr, err := p.server("")
	must.NoError(t, err)
	must.NoError(t, p.dial(addr, leafStolen, keyStolen, stolen))
	must.NoError(t, p.dial(addr, leafGood, keyGood, good))

	// The stolen node CA is distrusted: its nodes are refused, the others are not.
	list := p.write("distrusted", nil, stolen)
	addr, err = p.server(list)
	must.NoError(t, err)
	must.Error(t, p.dial(addr, leafStolen, keyStolen, stolen))
	must.NoError(t, p.dial(addr, leafGood, keyGood, good))

	// The same key in a node CA issued again is refused too.
	again := p.nodeCA("node CA 1 again", stolenKey)
	leafAgain, keyAgain := p.leaf("client.global.norca", again, stolenKey)
	must.Error(t, p.dial(addr, leafAgain, keyAgain, again))

	// The list changes while the server runs: it is read again.
	must.NoError(t, os.WriteFile(list, nil, 0o600)) // not readable as a list: the last one stays
	must.Error(t, p.dial(addr, leafStolen, keyStolen, stolen))
	p.write("distrusted", nil, good)
	future := time.Now().Add(2 * time.Second)
	must.NoError(t, os.Chtimes(list, future, future))
	must.NoError(t, p.dial(addr, leafStolen, keyStolen, stolen))
	must.Error(t, p.dial(addr, leafGood, keyGood, good))
}

func TestDistrustedCA_BadFile(t *testing.T) {
	ci.Parallel(t)
	p := newTestPKI(t)

	// A list that is set must be in force: a missing or empty file is an
	// error of the configuration.
	_, err := p.server(filepath.Join(p.dir, "missing.pem"))
	must.ErrorContains(t, err, "distrusted CA file")
	empty := filepath.Join(p.dir, "empty.pem")
	must.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
	_, err = p.server(empty)
	must.ErrorContains(t, err, "no certificate")
}

func TestDistrustedCA_Checksum(t *testing.T) {
	ci.Parallel(t)
	p := newTestPKI(t)
	cert, key := p.leaf("server.global.norca", p.ca, p.caKey)
	base := config.TLSConfig{
		EnableRPC: true, CAFile: p.write("ca", nil, p.ca),
		CertFile: p.write("server", key, cert), KeyFile: filepath.Join(p.dir, "server-key.pem"),
	}
	with := base.Copy()
	with.DistrustedCAFile = p.write("distrusted", nil, p.ca)
	must.NoError(t, with.SetChecksum()) // as the agent does after it read the configuration

	// Setting the list changes the TLS of the server, so a reload applies it.
	equal, err := base.Copy().CertificateInfoIsEqual(with)
	must.NoError(t, err)
	must.False(t, equal)
	must.Eq(t, with.DistrustedCAFile, base.Copy().Merge(with).DistrustedCAFile)
}

func TestChainPeerVerifiers(t *testing.T) {
	ci.Parallel(t)
	var calls []string
	ok := func(name string) PeerVerifier {
		return func([][]byte, [][]*x509.Certificate) error { calls = append(calls, name); return nil }
	}
	fail := func([][]byte, [][]*x509.Certificate) error {
		calls = append(calls, "fail")
		return errors.New("refused")
	}

	must.NoError(t, ChainPeerVerifiers(nil, ok("a"), ok("b"))(nil, nil))
	must.Eq(t, []string{"a", "b"}, calls)

	calls = nil
	must.ErrorContains(t, ChainPeerVerifiers(ok("a"), fail, ok("b"))(nil, nil), "refused")
	must.Eq(t, []string{"a", "fail"}, calls) // stops at the first error
}
