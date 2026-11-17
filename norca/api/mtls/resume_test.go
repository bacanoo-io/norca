// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtls

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/shoenig/test/must"
)

// lockedIntentions is an IntentionSource that changes while connections are
// made.
type lockedIntentions struct {
	mu   sync.Mutex
	list []*api.ServiceIntention
}

func (l *lockedIntentions) Intentions() []*api.ServiceIntention {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.list
}

func (l *lockedIntentions) set(list ...*api.ServiceIntention) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = list
}

// echoServer answers a line with "echo <line>" on every connection.
func echoServer(t *testing.T, conf *tls.Config) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", conf)
	must.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err == nil {
					_, _ = conn.Write([]byte("echo " + line))
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// echo makes a connection and a round trip on it. It reports whether the
// handshake resumed a session. With TLS 1.3 the verdict of the server on the
// client arrives after the handshake of the client: the round trip brings it.
func echo(addr string, conf *tls.Config) (resumed bool, err error) {
	conn, err := tls.Dial("tcp", addr, conf)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	resumed = conn.ConnectionState().DidResume
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		return resumed, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return resumed, err
	}
	if line != "echo hello\n" {
		return resumed, errors.New("unexpected reply " + line)
	}
	return resumed, nil
}

// resumeSources returns the sources of a client workload (web) and a server
// workload (api) of one CA, and the directory of the files of the server.
func resumeSources(t *testing.T) (web, apiSrc *FileSource, apiDir string) {
	t.Helper()
	ca := newTestCA(t, "a.nomad")
	apiDir = t.TempDir()
	certPEM, keyPEM := ca.issue(t, "spiffe://a.nomad/ns/prod/job/web/group/fe", []string{"web.service.prod.norca"}, time.Now().Add(time.Hour))
	web, err := NewFileSource(writeFiles(t, t.TempDir(), certPEM, keyPEM, ca.pem))
	must.NoError(t, err)
	certPEM, keyPEM = ca.issue(t, "spiffe://a.nomad/ns/prod/job/api/group/be", []string{"api.service.prod.norca"}, time.Now().Add(time.Hour))
	apiSrc, err = NewFileSource(writeFiles(t, apiDir, certPEM, keyPEM, ca.pem))
	must.NoError(t, err)
	return web, apiSrc, apiDir
}

// readPair reads the certificate and the key of the files of a source.
func readPair(t *testing.T, dir string) ([]byte, []byte) {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFile))
	must.NoError(t, err)
	keyPEM, err := os.ReadFile(filepath.Join(dir, KeyFile))
	must.NoError(t, err)
	return certPEM, keyPEM
}

var (
	allowWeb = &api.ServiceIntention{Namespace: "prod", SourceNamespace: "prod", Source: "web", Destination: "api", Action: "allow"}
	denyWeb  = &api.ServiceIntention{Namespace: "prod", SourceNamespace: "prod", Source: "web", Destination: "api", Action: "deny"}
)

// A resumed TLS session is authorized again: a client that was let in once
// does not come back with its session ticket after a deny intention.
func TestServerConfig_ResumedSessionIsAuthorized(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			web, apiSrc, _ := resumeSources(t)
			intentions := &lockedIntentions{}
			intentions.set(allowWeb)
			addr := echoServer(t, apiSrc.ServerConfig(&Authorizer{Namespace: "prod", Intentions: intentions}, ""))

			client := web.ClientConfig(ExpectService("prod", "api"))
			client.MaxVersion = version
			client.ClientSessionCache = tls.NewLRUClientSessionCache(4)

			resumed, err := echo(addr, client)
			must.NoError(t, err)
			must.False(t, resumed)

			// The session is resumed while the intention allows.
			resumed, err = echo(addr, client)
			must.NoError(t, err)
			must.True(t, resumed, must.Sprint("the test needs a resumed handshake"))

			intentions.set(allowWeb, denyWeb)
			resumed, err = echo(addr, client)
			must.Error(t, err, must.Sprintf("resumed=%v", resumed))

			// A new handshake is refused as well, and both pass again
			// without the deny.
			fresh := web.ClientConfig(ExpectService("prod", "api"))
			fresh.MaxVersion = version
			_, err = echo(addr, fresh)
			must.Error(t, err)
			intentions.set(allowWeb)
			_, err = echo(addr, client)
			must.NoError(t, err)
		})
	}
}

// A resumed TLS session is verified against the trust bundle of the moment:
// a CA taken out of the bundle shuts out the sessions of its certificates.
func TestServerConfig_ResumedSessionIsVerified(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			web, apiSrc, apiDir := resumeSources(t)
			addr := echoServer(t, apiSrc.ServerConfig(nil, ""))

			client := web.ClientConfig(nil)
			client.MaxVersion = version
			client.ClientSessionCache = tls.NewLRUClientSessionCache(4)

			_, err := echo(addr, client)
			must.NoError(t, err)
			resumed, err := echo(addr, client)
			must.NoError(t, err)
			must.True(t, resumed, must.Sprint("the test needs a resumed handshake"))

			// The bundle of the server changes to another CA; its own
			// certificate stays.
			other := newTestCA(t, "a.nomad")
			certPEM, keyPEM := readPair(t, apiDir)
			writeFiles(t, apiDir, certPEM, keyPEM, other.pem)
			must.NoError(t, apiSrc.Reload())

			resumed, err = echo(addr, client)
			must.Error(t, err, must.Sprintf("resumed=%v", resumed))
		})
	}
}

// The client checks the server of a resumed session again too: the bundle
// and the verify function.
func TestClientConfig_ResumedSessionIsVerified(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			web, apiSrc, _ := resumeSources(t)
			addr := echoServer(t, apiSrc.ServerConfig(nil, ""))

			var mu sync.Mutex
			var refuse bool
			calls := 0
			client := web.ClientConfig(func(peer *Peer) error {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if refuse {
					return errors.New("refused by the test")
				}
				return ExpectService("prod", "api")(peer)
			})
			client.MaxVersion = version
			client.ClientSessionCache = tls.NewLRUClientSessionCache(4)

			_, err := echo(addr, client)
			must.NoError(t, err)
			resumed, err := echo(addr, client)
			must.NoError(t, err)
			must.True(t, resumed, must.Sprint("the test needs a resumed handshake"))
			mu.Lock()
			must.Eq(t, 2, calls)
			refuse = true
			mu.Unlock()

			_, err = echo(addr, client)
			must.ErrorContains(t, err, "refused by the test")
		})
	}
}
