// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package pool

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/shoenig/test/must"
)

func newTestPool(t *testing.T) *ConnPool {
	l := testlog.HCLogger(t)
	p := NewPool(l, 1*time.Minute, 10, nil)
	return p
}

func TestConnPool_ConnListener(t *testing.T) {
	ports := ci.PortAllocator.Grab(1)
	addrStr := fmt.Sprintf("127.0.0.1:%d", ports[0])
	addr, err := net.ResolveTCPAddr("tcp", addrStr)
	must.NoError(t, err)

	exitCh := make(chan struct{})
	defer close(exitCh)
	go func() {
		ln, listenErr := net.Listen("tcp", addrStr)
		must.NoError(t, listenErr)
		defer func() { _ = ln.Close() }()
		conn, _ := ln.Accept()
		defer func() { _ = conn.Close() }()
		<-exitCh
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a test pool
	pool := newTestPool(t)

	// Setup a listener
	c := make(chan *Conn, 1)
	pool.SetConnListener(c)

	// Make an RPC
	_, err = pool.acquire("test", addr)
	must.NoError(t, err)

	// Assert we get a connection.
	select {
	case <-c:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("timeout")
	}

	// Test that the channel is closed when the pool shuts down.
	err = pool.Shutdown()
	must.NoError(t, err)

	_, ok := <-c
	must.False(t, ok)
}

// A thread that waits for the connection attempt of another thread gets the
// error of that attempt, not only that there is no connection.
func TestConnPool_WaiterGetsTheErrorOfTheLead(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4647}
	for _, tc := range []struct {
		name     string
		leadErr  error
		contains []string
	}{
		{"the dial failed", errors.New("dial tcp 127.0.0.1:4647: connect: connection refused"),
			[]string{"lead thread didn't get connection", "connection refused"}},
		{"the connection was cleared", nil,
			[]string{"lead thread got a connection, but it was closed before it could be used"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			// A lead thread is dialing: acquire waits for it, and reads its
			// error once it is done.
			attempt := &dialAttempt{done: make(chan struct{})}
			pool.Lock()
			pool.limiter[addr.String()] = attempt
			pool.Unlock()

			// The attempt ends; it stays in the limiter until the waiter is
			// done, so that the waiter finds it whenever it starts.
			pool.Lock()
			attempt.err = tc.leadErr
			close(attempt.done)
			pool.Unlock()
			errCh := make(chan error, 1)
			go func() {
				_, err := pool.acquire("test", addr)
				errCh <- err
			}()
			err := <-errCh
			pool.Lock()
			delete(pool.limiter, addr.String())
			pool.Unlock()
			must.Error(t, err)
			for _, s := range tc.contains {
				must.StrContains(t, err.Error(), s)
			}
			if tc.leadErr != nil {
				must.ErrorIs(t, err, tc.leadErr)
			}
		})
	}
}
