// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package connprefix

import (
	"net"
	"net/http"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/hashicorp/go-connlimit"
	"github.com/shoenig/test/must"
)

type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c *addrConn) RemoteAddr() net.Addr { return c.remote }

func tcpConn(ip string) *addrConn {
	return &addrConn{remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000, Zone: "eth0"}}
}

func TestForLimit_RemoteAddr(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		remote net.Addr
		want   string
	}{
		{&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:3:4:5:6"), Port: 1, Zone: "eth0"}, "[2001:db8:1:2::]:0"},
		{&net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "eth0"}, "[fe80::]:0"},
		{&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1}, "192.0.2.1:1"},
		{&net.TCPAddr{IP: net.ParseIP("192.0.2.1").To4(), Port: 1}, "192.0.2.1:1"},
		{&net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.1"), Port: 1}, "192.0.2.1:1"},
		{&net.TCPAddr{}, ":0"},
		{&net.UnixAddr{Name: "/run/norca.sock", Net: "unix"}, "/run/norca.sock"},
	}
	for _, tc := range cases {
		c := ForLimit(&addrConn{remote: tc.remote})
		must.Eq(t, tc.want, c.RemoteAddr().String(), must.Sprint(tc.remote))
	}

	must.Nil(t, ForLimit(&addrConn{}).RemoteAddr())
	must.Nil(t, ForLimit(&addrConn{remote: (*net.TCPAddr)(nil)}).RemoteAddr().(*net.TCPAddr))
}

func TestForLimit_Unwrap(t *testing.T) {
	ci.Parallel(t)

	c := tcpConn("2001:db8::1")
	must.True(t, ForLimit(c) == ForLimit(c))
	must.False(t, ForLimit(c) == ForLimit(tcpConn("2001:db8::1")))
	must.True(t, Unwrap(ForLimit(c)) == net.Conn(c))
	must.True(t, Unwrap(c) == net.Conn(c))
}

// The addresses of one /64 count against one limit, IPv4 addresses each
// against their own.
func TestForLimit_Limiter(t *testing.T) {
	ci.Parallel(t)

	l := connlimit.NewLimiter(connlimit.Config{MaxConnsPerClientIP: 1})

	accept := func(ip string) (func(), error) {
		return l.Accept(ForLimit(tcpConn(ip)))
	}
	free, err := accept("2001:db8:1:2::1")
	must.NoError(t, err)
	_, err = accept("2001:db8:1:2:ffff::2")
	must.ErrorIs(t, err, connlimit.ErrPerClientIPLimitReached)
	_, err = accept("2001:db8:1:3::1")
	must.NoError(t, err)
	free()
	_, err = accept("2001:db8:1:2::3")
	must.NoError(t, err)

	_, err = accept("192.0.2.1")
	must.NoError(t, err)
	_, err = accept("192.0.2.2")
	must.NoError(t, err)
	_, err = accept("192.0.2.1")
	must.ErrorIs(t, err, connlimit.ErrPerClientIPLimitReached)
}

// The ConnState hook frees a connection of a /64 when it is closed.
func TestHTTPConnState(t *testing.T) {
	ci.Parallel(t)

	var rejected []net.Conn
	hook := HTTPConnState(connlimit.NewLimiter(connlimit.Config{MaxConnsPerClientIP: 1}).
		HTTPConnStateFuncWithErrorHandler(func(_ error, conn net.Conn) {
			rejected = append(rejected, Unwrap(conn))
		}))

	a, b, c := tcpConn("2001:db8:1:2::1"), tcpConn("2001:db8:1:2::2"), tcpConn("2001:db8:1:2::3")
	hook(a, http.StateNew)
	hook(b, http.StateNew)
	must.SliceLen(t, 1, rejected)
	must.True(t, rejected[0] == net.Conn(b))
	hook(b, http.StateClosed)
	hook(a, http.StateActive)
	hook(a, http.StateClosed)
	hook(c, http.StateNew)
	must.SliceLen(t, 1, rejected)
}
