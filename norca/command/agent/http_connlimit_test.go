// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// limitTestConn is a connection from an address that records whether it was
// closed. Only the methods the connection limiter calls are implemented.
type limitTestConn struct {
	net.Conn
	remote *net.TCPAddr
	closed atomic.Bool
}

func newLimitTestConn(ip string) *limitTestConn {
	return &limitTestConn{remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
}

func (c *limitTestConn) RemoteAddr() net.Addr              { return c.remote }
func (c *limitTestConn) Close() error                      { c.closed.Store(true); return nil }
func (c *limitTestConn) Write(b []byte) (int, error)       { return len(b), nil }
func (c *limitTestConn) SetDeadline(time.Time) error       { return nil }
func (c *limitTestConn) SetWriteDeadline(time.Time) error  { return nil }
func (c *limitTestConn) SetReadDeadline(t time.Time) error { return nil }

// hijackTestServer is an agent with http_max_conns_per_client and a handler
// that hijacks its connection, as the websocket of alloc exec does, and
// holds it until one side closes it.
type hijackTestServer struct {
	t      *testing.T
	agent  *TestAgent
	useTLS bool
	// held gets the server side of every hijacked connection
	held chan net.Conn
}

const hijackTestPath = "/v1/test/hijack"

func newHijackTestServer(t *testing.T, limit int, useTLS bool) *hijackTestServer {
	const (
		cafile  = "../../helper/tlsutil/testdata/norca-agent-ca.pem"
		foocert = "../../helper/tlsutil/testdata/regionFoo-server-norca.pem"
		fookey  = "../../helper/tlsutil/testdata/regionFoo-server-norca-key.pem"
	)
	h := &hijackTestServer{t: t, useTLS: useTLS, held: make(chan net.Conn, 64)}
	h.agent = makeHTTPServer(t, func(c *Config) {
		if useTLS {
			c.Region = "regionFoo"
			c.TLSConfig = &config.TLSConfig{
				EnableHTTP: true,
				CAFile:     cafile,
				CertFile:   foocert,
				KeyFile:    fookey,
			}
		}
		c.Limits.HTTPMaxConnsPerClient = pointer.Of(limit)
		c.LogLevel = "ERROR"
	})
	t.Cleanup(h.agent.Shutdown)

	h.agent.Server.mux.HandleFunc(hijackTestPath, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\nUpgrade: test\r\n\r\n")); err != nil {
			return
		}
		h.held <- conn
		// until the client or the test closes it
		io.Copy(io.Discard, conn)
	})
	return h
}

// dial opens a connection, with TLS after its handshake.
func (h *hijackTestServer) dial() (net.Conn, error) {
	addr := h.agent.Server.Addr
	if h.useTLS {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr,
			&tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return nil, err
		}
		h.t.Cleanup(func() { conn.Close() })
		return conn, nil
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	h.t.Cleanup(func() { conn.Close() })
	return conn, nil
}

// status returns the start of what the server sends on conn, the status
// line of a response, or the error of the read.
func (h *hijackTestServer) status(conn net.Conn, wait time.Duration) string {
	buf := make([]byte, len("HTTP/1.1 000"))
	must.NoError(h.t, conn.SetReadDeadline(time.Now().Add(wait)))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err.Error()
	}
	must.NoError(h.t, conn.SetReadDeadline(time.Time{}))
	return string(buf)
}

// tryHijack opens a connection that the handler hijacks. It returns the
// client side, the server side and the status the server answered with.
func (h *hijackTestServer) tryHijack() (client, server net.Conn, status string) {
	client, err := h.dial()
	if err != nil {
		return nil, nil, err.Error()
	}
	// a refused connection may be closed already: the status tells
	client.Write([]byte("GET " + hijackTestPath + " HTTP/1.1\r\nHost: test\r\n\r\n"))
	status = h.status(client, 10*time.Second)
	if status != "HTTP/1.1 101" {
		return client, nil, status
	}
	select {
	case server = <-h.held:
	case <-time.After(10 * time.Second):
		h.t.Fatal("the handler did not hijack the connection")
	}
	return client, server, status
}

func (h *hijackTestServer) hijack() (client, server net.Conn) {
	h.t.Helper()
	client, server, status := h.tryHijack()
	must.Eq(h.t, "HTTP/1.1 101", status)
	return client, server
}

// refused asserts that a new connection gets the answer of a client over
// its connection limit: 429, with TLS too (the server gave itself 10ms for
// the handshake and its answer, and this accepted a connection closed in or
// after the handshake as well; review finding MUK-34). A connection the
// server accepted stays open and silent, and the read fails with a timeout.
func (h *hijackTestServer) refused() {
	h.t.Helper()
	conn, err := h.dial()
	must.NoError(h.t, err)
	must.Eq(h.t, "HTTP/1.1 429", h.status(conn, 5*time.Second))
}

// A connection that a handler hijacked, as the websocket of alloc exec is,
// holds its slot of http_max_conns_per_client until it is closed: the limit
// bounds the open connections of a client, whoever serves them (review
// finding SEC-11c).
func TestHTTP_ConnLimit_Hijacked(t *testing.T) {
	ci.Parallel(t)

	const limit = 3

	for _, useTLS := range []bool{false, true} {
		name := "plain"
		if useTLS {
			name = "tls"
		}
		t.Run(name, func(t *testing.T) {
			ci.Parallel(t)

			h := newHijackTestServer(t, limit, useTLS)

			// hijacked connections fill the limit
			clients := make([]net.Conn, limit)
			servers := make([]net.Conn, limit)
			for i := range clients {
				clients[i], servers[i] = h.hijack()
			}
			h.refused()
			h.refused()

			// the server side closes one: its slot is free at once, and
			// the second close of the handler frees no other slot
			must.NoError(t, servers[0].Close())
			clients[0], servers[0] = h.hijack()
			h.refused()

			// the client side closes one: the slot is free when the
			// handler has seen it and closed its side
			must.NoError(t, clients[1].Close())
			must.Wait(t, wait.InitialSuccess(
				wait.ErrorFunc(func() error {
					c, s, status := h.tryHijack()
					if status != "HTTP/1.1 101" {
						return fmt.Errorf("expected a free slot, got %q", status)
					}
					clients[1], servers[1] = c, s
					return nil
				}),
				wait.Timeout(10*time.Second),
				wait.Gap(100*time.Millisecond),
			))
			h.refused()

			// hijacked and other connections share the limit: an idle
			// connection takes the slot of a closed hijacked one
			must.NoError(t, servers[2].Close())
			idle, err := h.dial()
			must.NoError(t, err)
			// the server counts it when it has accepted it, at the latest
			// when it answers a request on it
			_, err = idle.Write([]byte("GET /v1/agent/health HTTP/1.1\r\nHost: test\r\n\r\n"))
			must.NoError(t, err)
			must.Eq(t, "HTTP/1.1 200", h.status(idle, 10*time.Second))
			h.refused()
			_, _, status := h.tryHijack()
			must.NotEq(t, "HTTP/1.1 101", status)

			// no slot is lost by opening and closing: after many rounds
			// the client still has exactly its limit
			must.NoError(t, idle.Close())
			must.NoError(t, servers[0].Close())
			must.NoError(t, servers[1].Close())
			for round := 0; round < 20; round++ {
				var c, s net.Conn
				must.Wait(t, wait.InitialSuccess(
					wait.ErrorFunc(func() error {
						var status string
						c, s, status = h.tryHijack()
						if status != "HTTP/1.1 101" {
							return fmt.Errorf("round %d: expected a free slot, got %q", round, status)
						}
						return nil
					}),
					wait.Timeout(10*time.Second),
					wait.Gap(100*time.Millisecond),
				))
				if round%2 == 0 {
					must.NoError(t, s.Close())
				} else {
					must.NoError(t, c.Close())
				}
			}
			must.Wait(t, wait.InitialSuccess(
				wait.ErrorFunc(func() error {
					for i := range clients {
						c, s, status := h.tryHijack()
						if status != "HTTP/1.1 101" {
							for _, open := range servers[:i] {
								open.Close()
							}
							return fmt.Errorf("connection %d of %d: got %q", i+1, limit, status)
						}
						clients[i], servers[i] = c, s
					}
					return nil
				}),
				wait.Timeout(10*time.Second),
				wait.Gap(500*time.Millisecond),
			))
			h.refused()
		})
	}
}

// http_max_conns_per_client counts an IPv6 client by its /64: a host with a
// whole /64 can't open a connection from each of its addresses (review
// finding SEC-11b). IPv4 clients are counted by address.
func TestHTTP_ConnLimit_IPv6Prefix(t *testing.T) {
	ci.Parallel(t)

	connState := makeConnState(false, 0, 1, nil, testlog.HCLogger(t))
	must.NotNil(t, connState)

	open := func(ip string) *limitTestConn {
		c := newLimitTestConn(ip)
		connState(c, http.StateNew)
		return c
	}

	a := open("2001:db8:1:2::1")
	must.False(t, a.closed.Load())
	// another address of the same /64 is over the limit
	b := open("2001:db8:1:2:ffff::2")
	must.True(t, b.closed.Load())
	connState(b, http.StateClosed)
	// another /64 is not
	c := open("2001:db8:1:3::1")
	must.False(t, c.closed.Load())

	// once the first connection is closed, the /64 may open another one
	connState(a, http.StateClosed)
	d := open("2001:db8:1:2::3")
	must.False(t, d.closed.Load())

	// IPv4 addresses are counted one by one
	must.False(t, open("192.0.2.1").closed.Load())
	must.False(t, open("192.0.2.2").closed.Load())
	must.True(t, open("192.0.2.1").closed.Load())
}
