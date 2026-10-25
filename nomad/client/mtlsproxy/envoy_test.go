// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtlsproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

var update = flag.Bool("update", false, "update the golden files")

// checkGolden compares the files with testdata/<dir>, or rewrites it.
func checkGolden(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	golden := filepath.Join("testdata", dir)
	if *update {
		must.NoError(t, os.RemoveAll(golden))
		must.NoError(t, os.MkdirAll(golden, 0o755))
		for name, content := range files {
			must.NoError(t, os.WriteFile(filepath.Join(golden, name), content, 0o644))
		}
		return
	}
	entries, err := os.ReadDir(golden)
	must.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	var got []string
	for name := range files {
		got = append(got, name)
	}
	sort.Strings(got)
	must.Eq(t, names, got)
	for name, content := range files {
		want, err := os.ReadFile(filepath.Join(golden, name))
		must.NoError(t, err)
		// golden files may be checked out with CRLF line endings
		must.Eq(t, strings.ReplaceAll(string(want), "\r\n", "\n"), string(content), must.Sprintf("file %s", name))
	}
}

func testIdentity() *Identity {
	return &Identity{
		CertPEM: []byte("-----BEGIN CERTIFICATE-----\nWORKLOAD\n-----END CERTIFICATE-----\n"),
		KeyPEM:  []byte("-----BEGIN PRIVATE KEY-----\nKEY\n-----END PRIVATE KEY-----\n"),
		Bundles: []TrustBundle{
			{TrustDomain: "global.nomad", PEM: "-----BEGIN CERTIFICATE-----\nCA1\n-----END CERTIFICATE-----\n"},
			{TrustDomain: "other.nomad", PEM: "-----BEGIN CERTIFICATE-----\nCA2\n-----END CERTIFICATE-----\n"},
		},
	}
}

func testSidecar() *Sidecar {
	return &Sidecar{
		NodeID:      "alloc1-mtls-proxy-api",
		Cluster:     "api",
		ConfigDir:   "/secrets/envoy",
		Namespace:   "default",
		Service:     "api",
		Region:      "global",
		InboundPort: 25001,
		AppAddress:  "127.0.0.1",
		AppPort:     8080,
		Intentions: []*structs.ServiceIntention{
			{Namespace: "default", SourceNamespace: "default", Source: "web", Destination: "api", Action: "allow"},
			{Namespace: "default", SourceNamespace: "*", Source: "*", Destination: "*", Action: "deny"},
			{Namespace: "default", SourceNamespace: "default", Source: "batch", Destination: "api", Action: "deny"},
			{Namespace: "default", SourceNamespace: "prod", Source: "*", Destination: "api", Action: "allow"},
			{Namespace: "default", SourceNamespace: "default", Source: "web", Destination: "db", Action: "allow"},
			{Namespace: "other", SourceNamespace: "other", Source: "web", Destination: "api", Action: "allow"},
		},
		Upstreams: []Upstream{
			{
				Name: "db", LocalPort: 5432, Namespace: "default", Service: "postgres", Region: "global",
				Endpoints: []Endpoint{{Address: "10.0.0.2", Port: 24000}, {Address: "10.0.0.1", Port: 24001}, {Address: "10.0.0.2", Port: 24000}},
			},
			{
				Name: "remote-cache", LocalPort: 6379, Namespace: "cache", Service: "redis", Region: "eu",
				Endpoints: []Endpoint{{Address: "192.168.10.5", Port: 8443}},
			},
			{Name: "empty", LocalPort: 7000, Namespace: "default", Service: "nothing", Region: "global"},
		},
	}
}

func TestSidecar_Golden(t *testing.T) {
	ci.Parallel(t)

	files, err := testSidecar().Files(testIdentity())
	must.NoError(t, err)
	checkGolden(t, "sidecar", files)

	// every file is valid JSON
	for name, content := range files {
		var v any
		must.NoError(t, json.Unmarshal(content, &v), must.Sprintf("file %s", name))
	}

	// the output is deterministic
	again, err := testSidecar().Files(testIdentity())
	must.NoError(t, err)
	must.Eq(t, files, again)
}

func TestSidecar_NoIdentity(t *testing.T) {
	ci.Parallel(t)

	_, err := testSidecar().Files(nil)
	must.ErrorContains(t, err, "not available")
	_, err = testSidecar().Files(&Identity{CertPEM: []byte("x")})
	must.ErrorContains(t, err, "not available")
}

func TestGateway_Golden(t *testing.T) {
	ci.Parallel(t)

	g := &Gateway{
		NodeID:        "alloc2-mtls-gateway-gw",
		Cluster:       "mtls-gateway",
		ConfigDir:     "/secrets/envoy",
		Region:        "eu",
		ListenAddress: "0.0.0.0",
		ListenPort:    8443,
		Routes: []Route{
			{Namespace: "default", Service: "api", Endpoints: []Endpoint{{Address: "10.1.0.1", Port: 25001}}},
			{Namespace: "cache", Service: "Redis", Endpoints: []Endpoint{{Address: "10.1.0.2", Port: 25002}, {Address: "10.1.0.3", Port: 25003}}},
		},
	}
	files, err := g.Files()
	must.NoError(t, err)
	checkGolden(t, "gateway", files)

	empty := &Gateway{NodeID: "n", Cluster: "c", ConfigDir: "/secrets/envoy", Region: "eu", ListenAddress: "0.0.0.0", ListenPort: 8443}
	files, err = empty.Files()
	must.NoError(t, err)
	checkGolden(t, "gateway-empty", files)
}

func TestServiceName(t *testing.T) {
	ci.Parallel(t)
	must.Eq(t, "api.service.default.global.nomad", ServiceName("API", "default", "global"))
	// the name the servers write into the certificates
	must.SliceContains(t, structs.MTLSServiceDNSNames("api", "default", "global"), ServiceName("api", "default", "global"))
}

// testCA returns a self-signed CA certificate for a trust domain.
func testCA(t *testing.T, trustDomain string) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "Norca CA " + trustDomain},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		URIs:                  []*url.URL{{Scheme: "spiffe", Host: trustDomain}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	must.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}

func TestParseBundle(t *testing.T) {
	ci.Parallel(t)

	a1, _, _ := testCA(t, "global.nomad")
	a2, _, _ := testCA(t, "global.nomad")
	b, _, _ := testCA(t, "other.nomad")

	// a CA without a SPIFFE trust domain is skipped
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must.NoError(t, err)
	plain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	bundle := string(a1) + string(b) + string(plain) + string(a2)
	bundles, err := ParseBundle([]byte(bundle))
	must.NoError(t, err)
	must.Eq(t, []TrustBundle{
		{TrustDomain: "global.nomad", PEM: string(a1) + string(a2)},
		{TrustDomain: "other.nomad", PEM: string(b)},
	}, bundles)

	_, err = ParseBundle(plain)
	must.ErrorContains(t, err, "no SPIFFE")
	_, err = ParseBundle([]byte("garbage"))
	must.Error(t, err)
}

func TestFilterEndpoints(t *testing.T) {
	ci.Parallel(t)

	reg := func(address string, port int, mode string, healthy bool) *structs.ServiceRegistration {
		r := &structs.ServiceRegistration{Address: address, Port: port, Ready: true, Meta: map[string]string{}}
		if mode != "" {
			r.Meta[structs.MTLSServiceMetaKey] = mode
		}
		if !healthy {
			r.Health = structs.ServiceHealthCritical
		}
		return r
	}
	regs := []*structs.ServiceRegistration{
		reg("10.0.0.1", 1, structs.MTLSModeSidecar, true),
		reg("10.0.0.2", 2, structs.MTLSModeApp, true),
		reg("10.0.0.3", 3, structs.MTLSModeSidecar, false),
		reg("10.0.0.4", 4, "", true),
		reg("10.0.0.5", 5, structs.MTLSModeGateway, true),
		reg("", 6, structs.MTLSModeSidecar, true),
	}
	must.Eq(t, []Endpoint{{"10.0.0.1", 1}, {"10.0.0.2", 2}},
		FilterEndpoints(regs, []string{structs.MTLSModeApp, structs.MTLSModeSidecar}))
	must.Eq(t, []Endpoint{{"10.0.0.5", 5}}, FilterEndpoints(regs, []string{structs.MTLSModeGateway}))
}
