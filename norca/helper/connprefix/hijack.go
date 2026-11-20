// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package connprefix

import (
	"errors"
	"io"
	"net"
	"sync"
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
