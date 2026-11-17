// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

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
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"
)

// busPKI is a cluster CA with node CAs (intermediates) and the agent
// certificates they sign, written as the files of a TLS configuration.
type busPKI struct {
	t      *testing.T
	dir    string
	ca     *x509.Certificate
	caKey  crypto.Signer
	serial int64
}

// busNodeCA is an intermediate of the cluster CA.
type busNodeCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func newBusPKI(t *testing.T) *busPKI {
	p := &busPKI{t: t, dir: t.TempDir()}
	p.caKey = p.key()
	p.ca = p.sign(&x509.Certificate{
		Subject: pkix.Name{CommonName: "cluster CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}, p.caKey.Public(), nil, p.caKey)
	p.write("ca", p.ca)
	return p
}

func (p *busPKI) key() crypto.Signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(p.t, err)
	return k
}

// sign signs the template with the key of the parent (the template itself
// when parent is nil).
func (p *busPKI) sign(tmpl *x509.Certificate, pub crypto.PublicKey, parent *x509.Certificate, parentKey crypto.Signer) *x509.Certificate {
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

func (p *busPKI) nodeCA(name string) *busNodeCA {
	key := p.key()
	return &busNodeCA{key: key, cert: p.sign(&x509.Certificate{
		Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign,
	}, key.Public(), p.ca, p.caKey)}
}

// write writes the certificates as the PEM file <name>.pem and returns its
// path.
func (p *busPKI) write(name string, certs ...*x509.Certificate) string {
	var data []byte
	for _, c := range certs {
		data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	path := filepath.Join(p.dir, name+".pem")
	must.NoError(p.t, os.WriteFile(path, data, 0o600))
	return path
}

// agent issues a certificate of the node CA for the DNS name, written with
// the node CA after it and its key as <file>.pem and <file>-key.pem, and
// returns the TLS configuration of an agent that has it. distrusted is the
// distrusted_ca_file, or empty.
func (p *busPKI) agent(file, dnsName string, ca *busNodeCA, distrusted string) *config.TLSConfig {
	key := p.key()
	cert := p.sign(&x509.Certificate{
		Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName, "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}, key.Public(), ca.cert, ca.key)
	certFile := p.write(file, cert, ca.cert)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	must.NoError(p.t, err)
	keyFile := filepath.Join(p.dir, file+"-key.pem")
	must.NoError(p.t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	return &config.TLSConfig{
		EnableRPC:            true,
		VerifyServerHostname: true,
		CAFile:               filepath.Join(p.dir, "ca.pem"),
		CertFile:             certFile,
		KeyFile:              keyFile,
		DistrustedCAFile:     distrusted,
	}
}

// distrust writes the list of the distrusted CAs and makes sure the change is
// seen (the list is read again when the time or the size of the file changes).
func (p *busPKI) distrust(seq int, cas ...*busNodeCA) string {
	certs := make([]*x509.Certificate, 0, len(cas))
	for _, ca := range cas {
		certs = append(certs, ca.cert)
	}
	path := p.write("distrusted", certs...)
	stamp := time.Now().Add(time.Duration(seq) * 2 * time.Second)
	must.NoError(p.t, os.Chtimes(path, stamp, stamp))
	return path
}

// routeTLS returns the TLS configuration of the routes of a server.
func routeTLS(t *testing.T, conf *config.TLSConfig) *tls.Config {
	t.Helper()
	_, clusterTLS, err := busTLS(conf, "global")
	must.NoError(t, err)
	return clusterTLS
}

// routeHandshake makes a TLS connection as two servers of the bus do for a
// route: one accepts with its route configuration, the other dials with its
// own. It returns whether the handshake resumed a session and the error of
// each side. With TLS 1.3 the dialling side learns the verdict of the
// accepting side after its own handshake, with the byte it reads.
func routeHandshake(t *testing.T, accept, dial *tls.Config) (resumed bool, acceptErr, dialErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		tc := tls.Server(conn, accept)
		if err := tc.Handshake(); err != nil {
			done <- err
			return
		}
		_, err = tc.Write([]byte{1})
		done <- err
	}()

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, dialErr := tls.DialWithDialer(dialer, "tcp", ln.Addr().String(), dial)
	if dialErr == nil {
		resumed = conn.ConnectionState().DidResume
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		// The session ticket of TLS 1.3 arrives with this read too.
		_, dialErr = conn.Read(make([]byte, 1))
		conn.Close()
	}
	return resumed, <-done, dialErr
}

// TestBusTLS_RouteDistrustedCA checks that the distrusted CAs of the TLS
// configuration hold for the routes of the bus, in both directions and for a
// resumed session: the holder of a listed CA that is still in the bundle
// issues itself the certificate of a server of the region, and as a route it
// would be a full member of the bus.
func TestBusTLS_RouteDistrustedCA(t *testing.T) {
	ci.Parallel(t)
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			p := newBusPKI(t)
			good, stolen, other := p.nodeCA("node CA 1"), p.nodeCA("node CA 2"), p.nodeCA("node CA 3")

			// The server lists another CA at first. The holder of the CA
			// that will be listed has no list of its own.
			list := p.distrust(0, other)
			server := routeTLS(t, p.agent("server", "server.global.norca", good, list))
			peer := routeTLS(t, p.agent("peer", "server.global.norca", good, list))
			thief := routeTLS(t, p.agent("thief", "server.global.norca", stolen, ""))

			withCache := func(c *tls.Config) *tls.Config {
				c = c.Clone()
				c.MaxVersion = version
				c.ClientSessionCache = tls.NewLRUClientSessionCache(4)
				return c
			}
			fresh := func(c *tls.Config) *tls.Config {
				c = c.Clone()
				c.MaxVersion = version
				return c
			}
			// A session of each direction, made and resumed while the CA is
			// not listed.
			thiefDials, serverDials := withCache(thief), withCache(server)
			for _, hs := range []struct{ accept, dial *tls.Config }{{server, thiefDials}, {thief, serverDials}} {
				resumed, acceptErr, dialErr := routeHandshake(t, hs.accept, hs.dial)
				must.NoError(t, acceptErr)
				must.NoError(t, dialErr)
				must.False(t, resumed)
				resumed, acceptErr, dialErr = routeHandshake(t, hs.accept, hs.dial)
				must.NoError(t, acceptErr)
				must.NoError(t, dialErr)
				must.True(t, resumed, must.Sprint("the test needs a resumed handshake"))
			}

			// The CA is listed.
			p.distrust(1, stolen)

			// The accepting side refuses the certificate of the CA: a new
			// handshake and the resumed session.
			_, acceptErr, _ := routeHandshake(t, server, fresh(thief))
			must.ErrorContains(t, acceptErr, "distrusted CA")
			resumed, acceptErr, _ := routeHandshake(t, server, thiefDials)
			must.ErrorContains(t, acceptErr, "distrusted CA", must.Sprintf("resumed=%v", resumed))

			// The dialling side refuses it too.
			_, _, dialErr := routeHandshake(t, thief, fresh(server))
			must.ErrorContains(t, dialErr, "distrusted CA")
			resumed, _, dialErr = routeHandshake(t, thief, serverDials)
			must.ErrorContains(t, dialErr, "distrusted CA", must.Sprintf("resumed=%v", resumed))

			// The servers of a CA that is not listed still connect, both
			// ways.
			_, acceptErr, dialErr = routeHandshake(t, server, fresh(peer))
			must.NoError(t, acceptErr)
			must.NoError(t, dialErr)
			_, acceptErr, dialErr = routeHandshake(t, peer, fresh(server))
			must.NoError(t, acceptErr)
			must.NoError(t, dialErr)

			// The name of a server of the region is still required, both
			// ways: a client certificate of the same CA is no route.
			client := routeTLS(t, p.agent("client", "client.global.norca", good, ""))
			_, acceptErr, _ = routeHandshake(t, server, fresh(client))
			must.ErrorContains(t, acceptErr, "bus route")
			_, _, dialErr = routeHandshake(t, client, fresh(server))
			must.ErrorContains(t, dialErr, "bus route")
		})
	}
}

// startTLSBus starts a clustered bus server with the TLS of the agent
// configuration and returns it with the address of its route listener.
func startTLSBus(t *testing.T, name string, conf *config.TLSConfig) (*bus.Server, string) {
	t.Helper()
	clientTLS, clusterTLS, err := busTLS(conf, "global")
	must.NoError(t, err)
	cfg := bus.DefaultConfig()
	cfg.Enabled = true
	cfg.Clustered = true
	cfg.ServerName = name
	cfg.Region = "global"
	cfg.BindAddr = "127.0.0.1"
	// Windows may keep a JetStream file open for a moment after the
	// shutdown, which fails the cleanup of t.TempDir.
	store, err := os.MkdirTemp("", "norca-bus-test")
	must.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(store) })
	cfg.StoreDir = store
	cfg.MaxMemory, cfg.MaxStorage = 32<<20, 128<<20
	cfg.ConsulKVMaxBytes = 16 << 20
	cfg.Events.MaxBytes = 16 << 20
	cfg.ClientTLS, cfg.ClusterTLS = clientTLS, clusterTLS
	// A reserved port may still be taken by another test for a moment:
	// retried with new ports.
	for attempt := 1; ; attempt++ {
		ports := ci.PortAllocator.Grab(2)
		cfg.Port, cfg.ClusterPort = ports[0], ports[1]
		b, err := bus.New(cfg, testlog.HCLogger(t), func(c bus.Credentials) (*bus.Grant, error) {
			if c.Token == "ok" {
				return &bus.Grant{Kind: bus.KindToken, Name: "ok", All: true}, nil
			}
			return nil, errors.New("denied")
		})
		if err != nil && attempt < 3 {
			t.Logf("starting bus %s: %v; retrying with new ports", name, err)
			continue
		}
		must.NoError(t, err)
		t.Cleanup(b.Shutdown)
		return b, fmt.Sprintf("127.0.0.1:%d", ports[1])
	}
}

// busRouteNames returns the names of the servers the server has a route to.
func busRouteNames(t *testing.T, s *bus.Server) []string {
	t.Helper()
	st, err := s.Status()
	must.NoError(t, err)
	var names []string
	for _, r := range st.Routes {
		names = append(names, r.Server)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// TestBusServer_RouteDistrustedCA checks the same on a bus: three servers of
// a region form the cluster with a distrusted CA listed, and a fourth with
// the certificate of a server of the region issued by the listed CA gets no
// route, whichever side dials.
func TestBusServer_RouteDistrustedCA(t *testing.T) {
	ci.Parallel(t)
	p := newBusPKI(t)
	good, stolen := p.nodeCA("node CA 1"), p.nodeCA("node CA 2")
	list := p.distrust(0, stolen)

	names := []string{"s1", "s2", "s3", "s4"}
	servers := make([]*bus.Server, len(names))
	addrs := make([]string, len(names))
	for i, name := range names {
		conf := p.agent(name, "server.global.norca", good, list)
		if name == "s4" {
			conf = p.agent(name, "server.global.norca", stolen, "")
		}
		servers[i], addrs[i] = startTLSBus(t, name, conf)
	}
	// Every server dials every other one: s4 is dialled and dials.
	for i, s := range servers {
		var routes []string
		for j, addr := range addrs {
			if j != i {
				routes = append(routes, addr)
			}
		}
		must.NoError(t, s.SetRoutes(routes))
	}

	// The three servers form the cluster.
	waitBus(t, func() (bool, error) {
		for i, s := range servers[:3] {
			want := slices.DeleteFunc(slices.Clone(names[:3]), func(n string) bool { return n == names[i] })
			got := slices.DeleteFunc(busRouteNames(t, s), func(n string) bool { return n == "s4" })
			if !slices.Equal(want, got) {
				return false, fmt.Errorf("routes of %s: %v", names[i], got)
			}
		}
		st, err := servers[0].Status()
		if err != nil {
			return false, err
		}
		return st.JetStream != nil && st.JetStream.MetaLeader != "", errors.New("no meta leader")
	})

	// Give the refused routes time to be tried, from both sides.
	time.Sleep(2 * time.Second)
	for i, s := range servers[:3] {
		must.SliceNotContains(t, busRouteNames(t, s), "s4", must.Sprintf("routes of %s", names[i]))
	}
	must.SliceEmpty(t, busRouteNames(t, servers[3]))

	// The client listener asks for no certificate, and refuses one of the
	// listed CA when it is given.
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	connect := func(certFile string) error {
		tlsConf := &tls.Config{RootCAs: pool, ServerName: "server.global.norca", MinVersion: tls.VersionTLS12}
		if certFile != "" {
			cert, err := tls.LoadX509KeyPair(filepath.Join(p.dir, certFile+".pem"), filepath.Join(p.dir, certFile+"-key.pem"))
			must.NoError(t, err)
			tlsConf.Certificates = []tls.Certificate{cert}
		}
		nc, err := nats.Connect(servers[0].ClientURL(), nats.Token("ok"), nats.Secure(tlsConf))
		if err != nil {
			return err
		}
		nc.Close()
		return nil
	}
	must.NoError(t, connect(""))
	must.NoError(t, connect("s2"))
	must.Error(t, connect("s4"))
}
