// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package connprefix

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// TrackListener returns ln with connections that tell HTTPConnState when
// they are closed. An http.Server reports no state of a connection after
// http.StateHijacked, so the close of the connection itself is the only
// sign that a hijacked connection (a websocket) is gone.
//
// Wrap the listener that accepts from the network: under a TLS listener,
// not over it, because an http.Server looks for the *tls.Conn itself.
func TrackListener(ln net.Listener) net.Listener {
	return trackListener{Listener: ln}
}

type trackListener struct {
	net.Listener
}

func (l trackListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: conn}, nil
}

// trackedConn runs a function once, when the connection is closed.
type trackedConn struct {
	net.Conn

	mu      sync.Mutex
	closed  bool
	onClose func()
}

// Close closes the connection and then runs the function of afterClose.
// Only the first call runs it.
func (c *trackedConn) Close() error {
	err := c.Conn.Close()

	c.mu.Lock()
	f := c.onClose
	c.onClose = nil
	c.closed = true
	c.mu.Unlock()

	if f != nil {
		f()
	}
	return err
}

// afterClose makes Close run f, once. A connection that is closed already
// runs f at once.
func (c *trackedConn) afterClose(f func()) {
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.onClose = f
	}
	c.mu.Unlock()

	if closed {
		f()
	}
}

// ReadFrom keeps the io.ReaderFrom of the wrapped connection (sendfile on a
// TCP connection) for the http.Server, which looks for it on the connection
// it serves.
func (c *trackedConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	// without the ReadFrom of c, or io.Copy would call it again
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}

// CloseWrite keeps the CloseWrite of the wrapped connection, which the
// http.Server calls before it closes a connection with unread input.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// Detach takes the network connection out of conn, a connection of a
// TrackListener or a TLS connection that runs on one, and returns it. It
// returns nil for any other connection, and for one that is closed or
// detached already.
//
// From then on conn is a closed connection for whoever holds it: its reads
// and writes fail and its Close does not reach the network connection,
// which is the caller's to use and to close. It is for a connection that an
// http.Server must not serve but that still gets an answer, which the
// caller then writes without the server waiting for it.
//
// Call it only before anything read or wrote conn, as in the http.StateNew
// hook: an http.Server starts to serve a connection after that hook
// returned. Detach does not synchronize with a use of conn at the same time.
func Detach(conn net.Conn) net.Conn {
	tc := tracked(conn)
	if tc == nil {
		return nil
	}

	tc.mu.Lock()
	defer tc.mu.Unlock()
	if _, ok := tc.Conn.(detachedConn); ok || tc.closed {
		return nil
	}
	nc := tc.Conn
	tc.Conn = detachedConn{local: nc.LocalAddr(), remote: nc.RemoteAddr()}
	return nc
}

// detachedConn is what Detach leaves in a trackedConn: a closed connection
// that still knows its addresses, which a connection limiter keeps a
// connection by.
type detachedConn struct {
	local, remote net.Addr
}

func (detachedConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (detachedConn) Write([]byte) (int, error)        { return 0, net.ErrClosed }
func (detachedConn) Close() error                     { return nil }
func (c detachedConn) LocalAddr() net.Addr            { return c.local }
func (c detachedConn) RemoteAddr() net.Addr           { return c.remote }
func (detachedConn) SetDeadline(time.Time) error      { return net.ErrClosed }
func (detachedConn) SetReadDeadline(time.Time) error  { return net.ErrClosed }
func (detachedConn) SetWriteDeadline(time.Time) error { return net.ErrClosed }

// tracked returns the connection of a TrackListener that conn is, or that a
// TLS connection conn runs on; nil for any other connection.
func tracked(conn net.Conn) *trackedConn {
	if tc, ok := conn.(*trackedConn); ok {
		return tc
	}
	// *tls.Conn
	if nc, ok := conn.(interface{ NetConn() net.Conn }); ok {
		if tc, ok := nc.NetConn().(*trackedConn); ok {
			return tc
		}
	}
	return nil
}
