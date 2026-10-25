// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtls

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/shoenig/test/must"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, trustDomain string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "test CA " + trustDomain},
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: trustDomain}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	must.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	must.NoError(t, err)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns the PEM certificate and key of a workload certificate.
func (ca *testCA) issue(t *testing.T, id string, dns []string, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	u, err := url.Parse(id)
	must.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		URIs:         []*url.URL{u},
		DNSNames:     dns,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	must.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	must.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func parseCert(t *testing.T, data []byte) *x509.Certificate {
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	must.NoError(t, err)
	return cert
}

func TestParseID(t *testing.T) {
	id, err := ParseID("spiffe://global.nomad/ns/prod/job/web/group/frontend")
	must.NoError(t, err)
	must.Eq(t, ID{TrustDomain: "global.nomad", Namespace: "prod", Job: "web", Group: "frontend"}, id)
	must.Eq(t, "spiffe://global.nomad/ns/prod/job/web/group/frontend", id.String())

	for _, bad := range []string{
		"https://global.nomad/ns/prod/job/web/group/frontend",
		"spiffe://Global.nomad/ns/prod/job/web/group/frontend",
		"spiffe://global.nomad/ns/prod/job/web",
		"spiffe://global.nomad/ns/prod/job/web/group/frontend/extra",
		"spiffe://global.nomad/namespace/prod/job/web/group/frontend",
		"spiffe://global.nomad/ns/prod/job/w%2Fb/group/frontend",
		"spiffe://global.nomad/ns/prod/job/../group/frontend",
		"spiffe://global.nomad:8443/ns/prod/job/web/group/frontend",
		"spiffe://global.nomad/ns/prod/job/web/group/frontend?x=1",
		"spiffe://user@global.nomad/ns/prod/job/web/group/frontend",
	} {
		_, err := ParseID(bad)
		must.Error(t, err, must.Sprint(bad))
	}
}

func TestServicesFromCertificate(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{
		"api.service.prod.nomad",
		"api.service.prod.eu.nomad",
		"admin.service.prod.nomad",
		"other.service.dev.nomad",
		"short.service.nomad",
		"example.com",
	}}
	must.Eq(t, []string{"api", "admin"}, ServicesFromCertificate(cert, "prod"))
	must.Eq(t, []string{"short"}, ServicesFromCertificate(cert, "default"))
}

func TestEvaluate(t *testing.T) {
	in := func(ns, src, dst, action string) *api.ServiceIntention {
		return &api.ServiceIntention{Namespace: "prod", SourceNamespace: ns, Source: src, Destination: dst, Action: action}
	}
	web := Source{Namespace: "prod", Services: []string{"web"}}

	cases := []struct {
		name       string
		intentions []*api.ServiceIntention
		src        Source
		dst        string
		dflt       bool
		allowed    bool
	}{
		{name: "default deny", src: web, dst: "api"},
		{name: "default allow", src: web, dst: "api", dflt: true, allowed: true},
		{name: "exact allow", intentions: []*api.ServiceIntention{in("prod", "web", "api", "allow")}, src: web, dst: "api", allowed: true},
		{name: "other destination", intentions: []*api.ServiceIntention{in("prod", "web", "db", "allow")}, src: web, dst: "api"},
		{name: "exact beats wildcard deny", allowed: true, src: web, dst: "api",
			intentions: []*api.ServiceIntention{in("prod", "*", "api", "deny"), in("prod", "web", "api", "allow")}},
		{name: "exact deny beats wildcard allow", src: web, dst: "api", dflt: true,
			intentions: []*api.ServiceIntention{in("*", "*", "*", "allow"), in("prod", "web", "api", "deny")}},
		{name: "deny wins at equal specificity", src: Source{Namespace: "prod", Services: []string{"web", "metrics"}}, dst: "api",
			intentions: []*api.ServiceIntention{in("prod", "web", "api", "allow"), in("prod", "metrics", "api", "deny")}},
		{name: "exact destination beats exact source", src: web, dst: "api", allowed: true,
			intentions: []*api.ServiceIntention{in("prod", "web", "*", "deny"), in("prod", "*", "api", "allow")}},
		{name: "source namespace", src: Source{Namespace: "dev", Services: []string{"web"}}, dst: "api",
			intentions: []*api.ServiceIntention{in("prod", "web", "api", "allow")}},
		{name: "wildcard source namespace", src: Source{Namespace: "dev", Services: []string{"web"}}, dst: "api", allowed: true,
			intentions: []*api.ServiceIntention{in("*", "web", "api", "allow")}},
		{name: "empty source namespace means the destination namespace", src: web, dst: "api", allowed: true,
			intentions: []*api.ServiceIntention{in("", "web", "api", "allow")}},
		{name: "source without services needs a wildcard", src: Source{Namespace: "prod"}, dst: "api", allowed: true,
			intentions: []*api.ServiceIntention{in("prod", "web", "api", "deny"), in("prod", "*", "api", "allow")}},
		{name: "case insensitive services", src: web, dst: "API", allowed: true,
			intentions: []*api.ServiceIntention{in("prod", "Web", "api", "allow")}},
		{name: "other namespace intentions ignored", src: web, dst: "api",
			intentions: []*api.ServiceIntention{{Namespace: "dev", SourceNamespace: "prod", Source: "web", Destination: "api", Action: "allow"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.intentions, tc.src, "prod", tc.dst, tc.dflt)
			must.Eq(t, tc.allowed, d.Allowed)
		})
	}
}

func TestBundle_Verify(t *testing.T) {
	caA := newTestCA(t, "a.nomad")
	caB := newTestCA(t, "b.nomad")
	other := newTestCA(t, "a.nomad")
	bundle, err := ParseBundle(append(append([]byte{}, caA.pem...), caB.pem...))
	must.NoError(t, err)
	must.Eq(t, []string{"a.nomad", "b.nomad"}, bundle.TrustDomains())

	certPEM, _ := caA.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", []string{"web.service.prod.nomad"}, time.Now().Add(time.Hour))
	peer, err := bundle.Verify([]*x509.Certificate{parseCert(t, certPEM)})
	must.NoError(t, err)
	must.Eq(t, "web", peer.ID.Job)
	must.Eq(t, []string{"web"}, peer.Services)

	// A CA is only trusted for its own trust domain.
	certPEM, _ = caB.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", nil, time.Now().Add(time.Hour))
	_, err = bundle.Verify([]*x509.Certificate{parseCert(t, certPEM)})
	must.ErrorContains(t, err, "not valid")

	// Unknown CA and unknown trust domain.
	certPEM, _ = other.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", nil, time.Now().Add(time.Hour))
	_, err = bundle.Verify([]*x509.Certificate{parseCert(t, certPEM)})
	must.Error(t, err)
	certPEM, _ = caA.issue(t, "spiffe://c.nomad/ns/prod/job/web/group/fe", nil, time.Now().Add(time.Hour))
	_, err = bundle.Verify([]*x509.Certificate{parseCert(t, certPEM)})
	must.ErrorContains(t, err, "not in the trust bundle")

	// Expired.
	certPEM, _ = caA.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", nil, time.Now().Add(time.Hour))
	_, err = bundle.verifyAt([]*x509.Certificate{parseCert(t, certPEM)}, time.Now().Add(2*time.Hour))
	must.ErrorContains(t, err, "not valid")

	_, err = ParseBundle([]byte("garbage"))
	must.Error(t, err)
}

// writeFiles writes a workload certificate, key and bundle into dir.
func writeFiles(t *testing.T, dir string, certPEM, keyPEM, bundle []byte) (string, string, string) {
	t.Helper()
	cert, key, ca := filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile), filepath.Join(dir, CAFile)
	must.NoError(t, os.WriteFile(key, keyPEM, 0o600))
	must.NoError(t, os.WriteFile(cert, certPEM, 0o600))
	must.NoError(t, os.WriteFile(ca, bundle, 0o600))
	return cert, key, ca
}

func TestFileSource_ReloadAndHandshake(t *testing.T) {
	ca := newTestCA(t, "a.nomad")
	webDir, apiDir := t.TempDir(), t.TempDir()

	certPEM, keyPEM := ca.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", []string{"web.service.prod.nomad"}, time.Now().Add(time.Hour))
	web, err := NewFileSource(writeFiles(t, webDir, certPEM, keyPEM, ca.pem))
	must.NoError(t, err)
	certPEM, keyPEM = ca.issue(t, "spiffe://a.nomad/ns/prod/job/api/group/be", []string{"api.service.prod.nomad"}, time.Now().Add(time.Hour))
	apiSrc, err := NewFileSource(writeFiles(t, apiDir, certPEM, keyPEM, ca.pem))
	must.NoError(t, err)
	must.Eq(t, []string{"api"}, apiSrc.Services())

	intentions := StaticIntentions{{Namespace: "prod", SourceNamespace: "prod", Source: "web", Destination: "api", Action: "allow"}}
	authz := &Authorizer{Namespace: "prod", Intentions: &intentions}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", apiSrc.ServerConfig(authz, ""))
	must.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err == nil {
					_, _ = conn.Write([]byte("echo " + line))
				}
			}()
		}
	}()

	roundTrip := func(src *FileSource) error {
		conn, err := tls.Dial("tcp", ln.Addr().String(), src.ClientConfig(ExpectService("prod", "api")))
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte("hello\n")); err != nil {
			return err
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return err
		}
		if line != "echo hello\n" {
			return errors.New("unexpected reply " + line)
		}
		peer, err := PeerFromConnectionState(conn.ConnectionState())
		if err != nil {
			return err
		}
		if peer.ID.Job != "api" {
			return errors.New("unexpected peer " + peer.ID.String())
		}
		return nil
	}
	must.NoError(t, roundTrip(web))

	// A deny intention takes effect on the next connection.
	intentions = append(intentions, &api.ServiceIntention{Namespace: "prod", SourceNamespace: "prod", Source: "web", Destination: "api", Action: "deny"})
	must.Error(t, roundTrip(web))

	// The client checks the identity of the server.
	conn, err := tls.Dial("tcp", ln.Addr().String(), web.ClientConfig(ExpectService("prod", "db")))
	if err == nil {
		conn.Close()
	}
	must.ErrorContains(t, err, `is not service "db"`)

	// A renewed certificate is picked up from the files.
	old := web.Certificate()
	reloadInterval = 0
	defer func() { reloadInterval = 5 * time.Second }()
	certPEM, keyPEM = ca.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", []string{"web.service.prod.nomad", "web2.service.prod.nomad"}, time.Now().Add(2*time.Hour))
	writeFiles(t, webDir, certPEM, keyPEM, ca.pem)
	// Make sure the modification time differs on coarse file systems.
	future := time.Now().Add(time.Minute)
	must.NoError(t, os.Chtimes(filepath.Join(webDir, CertFile), future, future))
	must.NotEq(t, old.Leaf.SerialNumber.String(), web.Certificate().Leaf.SerialNumber.String())
	must.Eq(t, []string{"web", "web2"}, web.Services())

	// A client certificate of an untrusted CA is rejected.
	stranger := newTestCA(t, "a.nomad")
	certPEM, keyPEM = stranger.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", []string{"web.service.prod.nomad"}, time.Now().Add(time.Hour))
	bad, err := NewFileSource(writeFiles(t, t.TempDir(), certPEM, keyPEM, ca.pem))
	must.NoError(t, err)
	intentions = intentions[:1]
	must.Error(t, roundTrip(bad))
	must.NoError(t, roundTrip(web))
}

func TestNewFileSourceFromEnv(t *testing.T) {
	ca := newTestCA(t, "a.nomad")
	secrets := t.TempDir()
	dir := filepath.Join(secrets, Dir)
	must.NoError(t, os.MkdirAll(dir, 0o755))
	certPEM, keyPEM := ca.issue(t, "spiffe://a.nomad/ns/default/job/web/group/fe", []string{"web.service.nomad"}, time.Now().Add(time.Hour))
	writeFiles(t, dir, certPEM, keyPEM, ca.pem)

	t.Setenv(EnvCertFile, "")
	t.Setenv("NOMAD_SECRETS_DIR", secrets)
	src, err := NewFileSourceFromEnv()
	must.NoError(t, err)
	must.Eq(t, "web", src.ID().Job)
	must.Eq(t, []string{"web"}, src.Services())

	t.Setenv("NOMAD_SECRETS_DIR", "")
	_, err = NewFileSourceFromEnv()
	must.Error(t, err)
}

func TestAuthorizer_Errors(t *testing.T) {
	a := &Authorizer{Namespace: "prod"}
	err := a.Authorize(&Peer{ID: ID{TrustDomain: "a.nomad", Namespace: "prod", Job: "web", Group: "fe"}, Services: []string{"web"}}, "api")
	must.True(t, errors.Is(err, ErrDenied))
	must.ErrorContains(t, err, "no intention allows")
	must.True(t, errors.Is(a.Authorize(nil, "api"), ErrDenied))

	a.DefaultAllow = true
	must.NoError(t, a.Authorize(&Peer{ID: ID{Namespace: "prod"}}, "api"))
}
