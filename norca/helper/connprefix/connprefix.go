// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package connprefix makes the per-client connection limits of the HTTP and
// the RPC listeners count an IPv6 client by its /64: a host has a whole /64
// to connect from, so a limit per address does not bound it.
package connprefix

import (
	"net"
	"net/http"
)

// ipv6ClientBits is the prefix length of an IPv6 client.
const ipv6ClientBits = 64

// ForLimit returns conn as a per-client connection limiter should see it:
// its RemoteAddr is the /64 of an IPv6 TCP remote address, without port and
// zone, so that the addresses of one /64 count as one client. Any other
// remote address, IPv4 and IPv4-mapped IPv6 among them, is left as it is.
// Hand the result only to the limiter, and use conn for everything else.
//
// The result is a comparable value, equal for the same conn, so that a
// limiter that keeps its connections by the net.Conn, as the http.Server
// ConnState hook of go-connlimit does, finds a connection again on a later
// call.
func ForLimit(conn net.Conn) net.Conn {
	return limitConn{Conn: conn}
}

// Unwrap returns the connection that ForLimit wrapped, or conn.
func Unwrap(conn net.Conn) net.Conn {
	if c, ok := conn.(limitConn); ok {
		return c.Conn
	}
	return conn
}

// HTTPConnState returns an http.Server ConnState hook that calls the hook f
// of a connection limiter with ForLimit(conn).
func HTTPConnState(f func(net.Conn, http.ConnState)) func(net.Conn, http.ConnState) {
	return func(conn net.Conn, state http.ConnState) {
		f(ForLimit(conn), state)
	}
}

type limitConn struct {
	net.Conn
}

func (c limitConn) RemoteAddr() net.Addr {
	addr := c.Conn.RemoteAddr()
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || tcp == nil || tcp.IP.To4() != nil || len(tcp.IP) != net.IPv6len {
		return addr
	}
	return &net.TCPAddr{IP: tcp.IP.Mask(net.CIDRMask(ipv6ClientBits, 8*net.IPv6len))}
}
