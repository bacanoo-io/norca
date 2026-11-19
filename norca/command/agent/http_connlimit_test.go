// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/shoenig/test/must"
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

// http_max_conns_per_client counts an IPv6 client by its /64: a host with a
// whole /64 can't open a connection from each of its addresses (review
// finding SEC-11b). IPv4 clients are counted by address.
func TestHTTP_ConnLimit_IPv6Prefix(t *testing.T) {
	ci.Parallel(t)

	connState := makeConnState(false, 0, 1, testlog.HCLogger(t))
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
