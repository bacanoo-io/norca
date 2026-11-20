// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package connprefix

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/hashicorp/go-connlimit"
	"github.com/shoenig/test/must"
)

// closeConn is a connection from an address that counts its Close calls.
type closeConn struct {
	net.Conn
	remote net.Addr
	closes atomic.Int32
}

func (c *closeConn) RemoteAddr() net.Addr { return c.remote }
func (c *closeConn) Close() error         { c.closes.Add(1); return nil }

// connListener accepts the connections it is given.
type connListener struct {
	net.Listener
	conns chan net.Conn
}

func (l *connListener) Accept() (net.Conn, error) {
	conn, ok := <-l.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return conn, nil
}

// tlsLikeConn has its connection behind NetConn, as a *tls.Conn has.
type tlsLikeConn struct {
	net.Conn
}

func (c *tlsLikeConn) NetConn() net.Conn { return c.Conn }

// limitHook is the ConnState hook of a limiter that closes the connections
// over the limit, as the HTTP server of the agent does, behind a
// TrackListener.
type limitHook struct {
	t       *testing.T
	limiter *connlimit.Limiter
	hook    func(net.Conn, http.ConnState)
	inner   *connListener
	ln      net.Listener
	refused atomic.Int32
}

func newLimitHook(t *testing.T, limit int) *limitHook {
	h := &limitHook{
		t:       t,
		limiter: connlimit.NewLimiter(connlimit.Config{MaxConnsPerClientIP: limit}),
		inner:   &connListener{conns: make(chan net.Conn, 1)},
	}
	h.ln = TrackListener(h.inner)
	h.hook = HTTPConnState(h.limiter.HTTPConnStateFuncWithErrorHandler(func(_ error, conn net.Conn) {
		h.refused.Add(1)
		Unwrap(conn).Close()
	}))
	return h
}

// open accepts a connection from ip and reports whether the limit let it in.
func (h *limitHook) open(ip string) (net.Conn, bool) {
	h.inner.conns <- &closeConn{remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
	conn, err := h.ln.Accept()
	must.NoError(h.t, err)
	before := h.refused.Load()
	h.hook(conn, http.StateNew)
	if h.refused.Load() != before {
		// the http.Server reports the close of a connection it gave up
		h.hook(conn, http.StateClosed)
		return conn, false
	}
	return conn, true
}

// hijack is what the http.Server does when a handler takes conn over.
func (h *limitHook) hijack(conn net.Conn) {
	h.hook(conn, http.StateActive)
	h.hook(conn, http.StateHijacked)
}

func (h *limitHook) numOpen(conn net.Conn) int {
	return h.limiter.NumOpen(ForLimit(conn).RemoteAddr())
}

// A hijacked connection holds its slot of the limit of its client, an IPv6
// client being a /64, until it is closed (review finding SEC-11c).
func TestHTTPConnState_HijackedHoldsSlot(t *testing.T) {
	ci.Parallel(t)

	h := newLimitHook(t, 2)

	a, ok := h.open("2001:db8:1:2::1")
	must.True(t, ok)
	h.hijack(a)
	must.Eq(t, 1, h.numOpen(a))

	// a hijacked and a plain connection share the limit
	b, ok := h.open("2001:db8:1:2:ffff::2")
	must.True(t, ok)
	must.Eq(t, 2, h.numOpen(a))

	// a third one from the /64 is refused and closed
	c, ok := h.open("2001:db8:1:2::3")
	must.False(t, ok)
	must.Eq(t, 1, c.(*trackedConn).Conn.(*closeConn).closes.Load())
	must.Eq(t, 2, h.numOpen(a))

	// another /64 and an IPv4 client have their own limits
	_, ok = h.open("2001:db8:1:3::1")
	must.True(t, ok)
	_, ok = h.open("192.0.2.1")
	must.True(t, ok)

	// two hijacked ones fill the limit as well
	h.hijack(b)
	_, ok = h.open("2001:db8:1:2::4")
	must.False(t, ok)

	// the close of a hijacked connection frees its slot, once
	must.NoError(t, a.Close())
	must.Eq(t, 1, h.numOpen(a))
	must.NoError(t, a.Close())
	must.Eq(t, 1, h.numOpen(a))
	d, ok := h.open("2001:db8:1:2::5")
	must.True(t, ok)
	_, ok = h.open("2001:db8:1:2::6")
	must.False(t, ok)

	must.NoError(t, b.Close())
	must.NoError(t, d.Close())
	h.hook(d, http.StateClosed)
	must.Eq(t, 0, h.numOpen(a))
}

// The connection of a TLS listener is found behind the TLS connection.
func TestHTTPConnState_HijackedTLS(t *testing.T) {
	ci.Parallel(t)

	h := newLimitHook(t, 1)

	h.inner.conns <- &closeConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000}}
	raw, err := h.ln.Accept()
	must.NoError(t, err)
	conn := &tlsLikeConn{Conn: raw}

	h.hook(conn, http.StateNew)
	h.hijack(conn)
	must.Eq(t, 1, h.numOpen(conn))
	_, ok := h.open("192.0.2.1")
	must.False(t, ok)

	must.NoError(t, conn.Close())
	must.Eq(t, 0, h.numOpen(conn))
	_, ok = h.open("192.0.2.1")
	must.True(t, ok)
}

// A connection that is closed before the server reports it hijacked does
// not keep its slot.
func TestHTTPConnState_HijackedAfterClose(t *testing.T) {
	ci.Parallel(t)

	h := newLimitHook(t, 1)
	a, ok := h.open("192.0.2.1")
	must.True(t, ok)
	must.NoError(t, a.Close())
	h.hijack(a)
	must.Eq(t, 0, h.numOpen(a))
}

// A connection that does not come from a TrackListener can't tell when it
// is closed: it is freed when it is hijacked, as go-connlimit does.
func TestHTTPConnState_HijackedUntracked(t *testing.T) {
	ci.Parallel(t)

	h := newLimitHook(t, 1)
	a := &closeConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000}}
	h.hook(a, http.StateNew)
	must.Eq(t, 1, h.numOpen(a))
	h.hijack(a)
	must.Eq(t, 0, h.numOpen(a))
}

// No slot is left behind by connections that are hijacked and closed, from
// many goroutines at once.
func TestHTTPConnState_HijackedNoLeak(t *testing.T) {
	ci.Parallel(t)

	const workers, cycles = 8, 200

	limiter := connlimit.NewLimiter(connlimit.Config{MaxConnsPerClientIP: workers})
	var refused atomic.Int32
	hook := HTTPConnState(limiter.HTTPConnStateFuncWithErrorHandler(func(_ error, conn net.Conn) {
		refused.Add(1)
		Unwrap(conn).Close()
	}))
	remote := &net.TCPAddr{IP: net.ParseIP("2001:db8:1:2::1"), Port: 40000}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cycles; i++ {
				conn := &trackedConn{Conn: &closeConn{remote: remote}}
				hook(conn, http.StateNew)
				hook(conn, http.StateActive)

				// the handler's goroutine and another one close it
				// while the server reports the hijack
				done := make(chan struct{})
				go func() {
					defer close(done)
					conn.Close()
				}()
				hook(conn, http.StateHijacked)
				conn.Close()
				<-done
			}
		}()
	}
	wg.Wait()

	must.Eq(t, 0, refused.Load())
	must.Eq(t, 0, limiter.NumOpen(ForLimit(&closeConn{remote: remote}).RemoteAddr()))
}

// writerConn is a connection without ReadFrom and CloseWrite.
type writerConn struct {
	net.Conn
	sb strings.Builder
}

func (c *writerConn) Write(b []byte) (int, error) { return c.sb.Write(b) }

// readFromConn has the ReadFrom and the CloseWrite of a TCP connection.
type readFromConn struct {
	writerConn
	readFrom, closeWrite int
}

func (c *readFromConn) ReadFrom(r io.Reader) (int64, error) {
	c.readFrom++
	return io.Copy(&c.sb, r)
}

func (c *readFromConn) CloseWrite() error { c.closeWrite++; return nil }

// The http.Server finds the ReadFrom and the CloseWrite of the connection.
func TestTrackListener_ConnMethods(t *testing.T) {
	ci.Parallel(t)

	inner := &readFromConn{}
	conn := &trackedConn{Conn: inner}
	n, err := conn.ReadFrom(strings.NewReader("abc"))
	must.NoError(t, err)
	must.Eq(t, 3, n)
	must.Eq(t, "abc", inner.sb.String())
	must.Eq(t, 1, inner.readFrom)
	must.NoError(t, conn.CloseWrite())
	must.Eq(t, 1, inner.closeWrite)

	plain := &writerConn{}
	conn = &trackedConn{Conn: plain}
	n, err = conn.ReadFrom(strings.NewReader("abcd"))
	must.NoError(t, err)
	must.Eq(t, 4, n)
	must.Eq(t, "abcd", plain.sb.String())
	must.True(t, errors.Is(conn.CloseWrite(), errors.ErrUnsupported))
}

// The listener hands on the error of the wrapped one.
func TestTrackListener_AcceptError(t *testing.T) {
	ci.Parallel(t)

	inner := &connListener{conns: make(chan net.Conn)}
	close(inner.conns)
	conn, err := TrackListener(inner).Accept()
	must.ErrorIs(t, err, net.ErrClosed)
	must.Nil(t, conn)
}
