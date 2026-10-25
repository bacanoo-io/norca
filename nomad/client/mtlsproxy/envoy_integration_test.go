// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtlsproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// The tests in this file run a real Envoy binary. They are skipped when no
// envoy binary is on the PATH.

func envoyBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("envoy")
	if err != nil {
		t.Skip("envoy is not on the PATH, skipping the Envoy integration test")
	}
	return path
}

// issueLeaf signs a workload certificate like the servers do.
func issueLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, namespace, service string, region ...string) *Identity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	id, err := url.Parse(structs.MTLSWorkloadSPIFFEID("global.nomad", namespace, "job-"+service, "group"))
	must.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{id},
		DNSNames:     structs.MTLSServiceDNSNames(service, namespace, append(region, "global")[0]),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	must.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	must.NoError(t, err)
	return &Identity{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// echoServer answers every line with the line.
func echoServer(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func runEnvoy(t *testing.T, binary, dir string) {
	t.Helper()
	level := os.Getenv("NORCA_TEST_ENVOY_LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	// The log must not be in the configuration directory: Envoy watches it,
	// and at debug level it logs every file event, including those of its
	// own log, which feeds itself without end.
	logFile := filepath.Join(t.TempDir(), "envoy.log")
	out, err := os.Create(logFile)
	must.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "-c", filepath.Join(dir, BootstrapFile),
		"--disable-hot-restart", "-l", level)
	cmd.Stdout = out
	cmd.Stderr = out
	must.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		_ = out.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logFile)
			t.Logf("envoy log of %s:\n%s", dir, b)
		}
	})
}

// roundTrip sends a line through the upstream listener and returns whether
// it came back.
func roundTrip(port int) bool {
	ok, _ := roundTripErr(port)
	return ok
}

// roundTripErr is roundTrip with the reason of a failure.
func roundTripErr(port int) (bool, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read %q: %w", line, err)
	}
	if line != "hello\n" {
		return false, fmt.Errorf("unexpected answer %q", line)
	}
	return true, nil
}

func TestEnvoy_SidecarToSidecar(t *testing.T) {
	ci.Parallel(t)
	binary := envoyBinary(t)

	caPEM, ca, caKey := testCA(t, "global.nomad")
	bundles, err := ParseBundle(caPEM)
	must.NoError(t, err)

	apiID := issueLeaf(t, ca, caKey, "default", "api")
	apiID.Bundles = bundles
	webID := issueLeaf(t, ca, caKey, "default", "web")
	webID.Bundles = bundles

	appPort := echoServer(t)
	inboundPort := freePort(t)
	upstreamPort := freePort(t)

	apiDir, webDir := t.TempDir(), t.TempDir()
	allow := []*structs.ServiceIntention{
		{Namespace: "default", SourceNamespace: "default", Source: "web", Destination: "api", Action: "allow"},
	}
	api := &Sidecar{
		NodeID: "api", Cluster: "api", ConfigDir: apiDir,
		Namespace: "default", Service: "api", Region: "global",
		InboundPort: inboundPort, AppAddress: "127.0.0.1", AppPort: appPort,
		Intentions: allow,
	}
	web := &Sidecar{
		NodeID: "web", Cluster: "web", ConfigDir: webDir,
		Namespace: "default", Service: "web", Region: "global",
		InboundPort: freePort(t), AppAddress: "127.0.0.1", AppPort: freePort(t),
		Upstreams: []Upstream{{
			Name: "api", LocalPort: upstreamPort, Namespace: "default", Service: "api", Region: "global",
			Endpoints: []Endpoint{{Address: "127.0.0.1", Port: inboundPort}},
		}},
	}

	apiWriter, webWriter := &Writer{Dir: apiDir}, &Writer{Dir: webDir}
	write := func(w *Writer, s *Sidecar, id *Identity) {
		files, err := s.Files(id)
		must.NoError(t, err)
		_, err = w.Write(files)
		must.NoError(t, err)
	}
	write(apiWriter, api, apiID)
	write(webWriter, web, webID)

	t.Run("validate", func(t *testing.T) {
		out, err := exec.Command(binary, "--mode", "validate", "-c", filepath.Join(apiDir, BootstrapFile)).CombinedOutput()
		must.NoError(t, err, must.Sprintf("%s", out))
	})

	runEnvoy(t, binary, apiDir)
	runEnvoy(t, binary, webDir)

	// allowed by the intention
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("connection through the sidecars failed"))

	// removing the intention denies new connections (default deny)
	// without restarting Envoy
	api.Intentions = nil
	write(apiWriter, api, apiID)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return !roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("the deny intention was not applied"))

	// and allowed again
	api.Intentions = allow
	write(apiWriter, api, apiID)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("the allow intention was not applied again"))

	// a peer with a certificate of another service is not the upstream
	// the downstream asked for
	other := issueLeaf(t, ca, caKey, "default", "other")
	other.Bundles = bundles
	write(apiWriter, api, other)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return !roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("the server certificate was not verified"))

	// back to the right server certificate
	write(apiWriter, api, apiID)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("the server certificate was not restored"))

	// a client certificate of a CA outside the trust bundle is rejected by
	// the inbound listener, even with the right names
	_, foreignCA, foreignKey := testCA(t, "global.nomad")
	foreign := issueLeaf(t, foreignCA, foreignKey, "default", "web")
	foreign.Bundles = bundles
	write(webWriter, web, foreign)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return !roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("a client certificate of an unknown CA was accepted"))
}

func TestEnvoy_Gateway(t *testing.T) {
	ci.Parallel(t)
	binary := envoyBinary(t)

	caPEM, ca, caKey := testCA(t, "global.nomad")
	bundles, err := ParseBundle(caPEM)
	must.NoError(t, err)

	// api runs in region eu behind its gateway, web in region global
	apiID := issueLeaf(t, ca, caKey, "default", "api", "eu")
	apiID.Bundles = bundles
	webID := issueLeaf(t, ca, caKey, "default", "web")
	webID.Bundles = bundles

	appPort := echoServer(t)
	inboundPort := freePort(t)
	gatewayPort := freePort(t)
	upstreamPort := freePort(t)
	apiDir, gwDir, webDir := t.TempDir(), t.TempDir(), t.TempDir()

	api := &Sidecar{
		NodeID: "api", Cluster: "api", ConfigDir: apiDir,
		Namespace: "default", Service: "api", Region: "eu",
		InboundPort: inboundPort, AppAddress: "127.0.0.1", AppPort: appPort,
		Intentions: []*structs.ServiceIntention{
			{Namespace: "default", SourceNamespace: "default", Source: "web", Destination: "api", Action: "allow"},
		},
	}
	gateway := &Gateway{
		NodeID: "gw", Cluster: "mtls-gateway", ConfigDir: gwDir, Region: "eu",
		ListenAddress: "127.0.0.1", ListenPort: gatewayPort,
		Routes: []Route{{Namespace: "default", Service: "api", Endpoints: []Endpoint{{Address: "127.0.0.1", Port: inboundPort}}}},
	}
	web := &Sidecar{
		NodeID: "web", Cluster: "web", ConfigDir: webDir,
		Namespace: "default", Service: "web", Region: "global",
		InboundPort: freePort(t), AppAddress: "127.0.0.1", AppPort: freePort(t),
		Upstreams: []Upstream{{
			Name: "api", LocalPort: upstreamPort, Namespace: "default", Service: "api", Region: "eu",
			Endpoints: []Endpoint{{Address: "127.0.0.1", Port: gatewayPort}},
		}},
	}

	files, err := api.Files(apiID)
	must.NoError(t, err)
	_, err = (&Writer{Dir: apiDir}).Write(files)
	must.NoError(t, err)
	files, err = gateway.Files()
	must.NoError(t, err)
	_, err = (&Writer{Dir: gwDir}).Write(files)
	must.NoError(t, err)
	files, err = web.Files(webID)
	must.NoError(t, err)
	_, err = (&Writer{Dir: webDir}).Write(files)
	must.NoError(t, err)

	runEnvoy(t, binary, apiDir)
	runEnvoy(t, binary, gwDir)
	runEnvoy(t, binary, webDir)

	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("connection through the gateway failed"))

	// The gateway passes the TLS connection through: a client connecting
	// to it directly with the SNI name of api gets the certificate of the
	// sidecar of api, and a connection with another name is closed.
	pool := x509.NewCertPool()
	must.True(t, pool.AppendCertsFromPEM(caPEM))
	webPair, err := tls.X509KeyPair(webID.CertPEM, webID.KeyPEM)
	must.NoError(t, err)
	dialGateway := func(sni string) (*tls.Conn, error) {
		return tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp",
			net.JoinHostPort("127.0.0.1", strconv.Itoa(gatewayPort)), &tls.Config{
				ServerName:   sni,
				RootCAs:      pool,
				Certificates: []tls.Certificate{webPair},
			})
	}
	conn, err := dialGateway("api.service.default.eu.nomad")
	must.NoError(t, err)
	peer := conn.ConnectionState().PeerCertificates
	must.SliceNotEmpty(t, peer)
	must.SliceContains(t, peer[0].DNSNames, "api.service.default.eu.nomad")
	_ = conn.Close()
	_, err = dialGateway("db.service.default.eu.nomad")
	must.Error(t, err)

	// an unknown SNI name is closed by the gateway
	gateway.Routes = nil
	files, err = gateway.Files()
	must.NoError(t, err)
	_, err = (&Writer{Dir: gwDir}).Write(files)
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return !roundTrip(upstreamPort) }),
		wait.Timeout(20*time.Second),
		wait.Gap(200*time.Millisecond),
	), must.Sprint("the gateway still forwards a removed route"))
}
