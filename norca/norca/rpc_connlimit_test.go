// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pool"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
)

// pipeListener hands out the server ends of in-memory connections.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv6loopback, Port: 4647} }

// remoteAddrConn is the server end of a pipe with the address of a client.
type remoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *remoteAddrConn) RemoteAddr() net.Addr { return c.remote }

// rpcLimitTest runs the accept loop of an RPC handler with the connection
// limit on a pipeListener.
type rpcLimitTest struct {
	t  *testing.T
	ln *pipeListener
}

func newRPCLimitTest(t *testing.T, limit int) *rpcLimitTest {
	cfg := DefaultConfig()
	cfg.RPCMaxConnsPerClient = limit
	cfg.RPCHandshakeTimeout = 0
	ln := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	s := &Server{
		config:      cfg,
		logger:      testlog.HCLogger(t),
		rpcListener: ln,
		listenerCh:  make(chan struct{}),
	}
	r := newRpcHandler(s)
	ctx, cancel := context.WithCancel(context.Background())
	go r.listen(ctx)
	t.Cleanup(func() {
		cancel()
		ln.Close()
		<-s.listenerCh
	})
	return &rpcLimitTest{t: t, ln: ln}
}

// dial opens a connection from ip, sends first if it is not zero, and
// returns the client end.
func (r *rpcLimitTest) dial(ip string, first pool.RPCType) net.Conn {
	client, server := net.Pipe()
	r.t.Cleanup(func() { client.Close() })
	r.ln.conns <- &remoteAddrConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
	if first != 0 {
		client.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := client.Write([]byte{byte(first)})
		must.NoError(r.t, err)
	}
	return client
}

// closedByServer reports whether the server closed the connection within
// wait; a connection the server keeps open times out instead.
func closedByServer(t *testing.T, c net.Conn, wait time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(wait))
	_, err := c.Read(make([]byte, 1))
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return false
	}
	must.ErrorIs(t, err, io.EOF)
	return true
}

// rpc_max_conns_per_client counts an IPv6 client by its /64, for the RPC
// connections and for the streaming RPCs (review finding SEC-11b). IPv4
// clients are counted by address.
func TestRPC_ConnLimit_IPv6Prefix(t *testing.T) {
	ci.Parallel(t)

	t.Run("connections", func(t *testing.T) {
		r := newRPCLimitTest(t, 2)

		a := r.dial("2001:db8:1:2::1", 0)
		b := r.dial("2001:db8:1:2::2", 0)
		// a third address of the same /64 is over the limit
		c := r.dial("2001:db8:1:2:ffff:1:2:3", 0)
		must.True(t, closedByServer(t, c, 5*time.Second))
		// another /64 is not
		d := r.dial("2001:db8:1:3::1", 0)
		// IPv4 addresses are counted one by one
		e := r.dial("192.0.2.1", 0)
		f := r.dial("192.0.2.2", 0)
		g := r.dial("192.0.2.2", 0)
		h := r.dial("192.0.2.2", 0)
		must.True(t, closedByServer(t, h, 5*time.Second))

		for _, open := range []net.Conn{a, b, d, e, f, g} {
			must.False(t, closedByServer(t, open, 100*time.Millisecond))
		}
	})

	t.Run("streams", func(t *testing.T) {
		// one streaming connection per client
		r := newRPCLimitTest(t, config.LimitsNonStreamingConnsPerClient+1)

		a := r.dial("2001:db8:1:2::1", pool.RpcStreaming)
		b := r.dial("2001:db8:1:2::2", pool.RpcStreaming)
		must.True(t, closedByServer(t, b, 5*time.Second))
		c := r.dial("2001:db8:1:3::1", pool.RpcStreaming)

		for _, open := range []net.Conn{a, c} {
			must.False(t, closedByServer(t, open, 100*time.Millisecond))
		}
	})
}
