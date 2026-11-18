// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"net"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/client/servers"
	"github.com/bacanoo-io/norca/norca/helper/pool"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// newRPCTestClient returns a client with only what Client.RPC uses: no
// servers, not registered, not shut down.
func newRPCTestClient(t *testing.T) *Client {
	t.Helper()
	conf := config.DefaultConfig()
	conf.RPCHoldTimeout = 100 * time.Millisecond
	logger := testlog.HCLogger(t)
	c := &Client{
		config:       conf,
		logger:       logger,
		rpcLogger:    logger.Named("rpc"),
		registeredCh: make(chan struct{}),
		shutdownCh:   make(chan struct{}),
	}
	c.servers = servers.New(logger, c.shutdownCh, c)
	c.connPool = pool.NewPool(logger, time.Minute, 10, nil)
	t.Cleanup(func() { _ = c.connPool.Shutdown() })
	return c
}

// closedAddr returns a TCP address nothing listens on.
func closedAddr(t *testing.T) net.Addr {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	addr := l.Addr()
	must.NoError(t, l.Close())
	return addr
}

// TestRPC_ShutdownBeforeRegistration: a call that waits for the node
// registration when the client shuts down fails; it was not made, and a nil
// error would let the caller use the empty reply as the servers' answer.
func TestRPC_ShutdownBeforeRegistration(t *testing.T) {
	ci.Parallel(t)
	c := newRPCTestClient(t)
	close(c.shutdownCh)

	var reply structs.ACLWhoAmIResponse
	err := c.RPC("ACL.WhoAmI", &structs.GenericRequest{}, &reply)
	must.ErrorIs(t, err, errShuttingDown)
}

// callRPC makes the call on a goroutine and fails the test if it does not
// return within the wait.
func callRPC(t *testing.T, c *Client, wait time.Duration) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		var reply structs.ACLWhoAmIResponse
		errCh <- c.RPC("ACL.WhoAmI", &structs.GenericRequest{}, &reply)
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(wait):
		t.Fatalf("RPC still waiting for the node registration after %s", wait)
		return nil
	}
}

// TestRPC_RegistrationWaitIsBounded: a call made before the node registered
// waits for the registration no longer than the RPC hold timeout, and then
// fails, instead of waiting as long as the servers are unreachable (review
// finding MUK-26).
func TestRPC_RegistrationWaitIsBounded(t *testing.T) {
	ci.Parallel(t)

	t.Run("no servers", func(t *testing.T) {
		c := newRPCTestClient(t)
		err := callRPC(t, c, 5*time.Second)
		must.ErrorIs(t, err, noServersErr)
	})

	t.Run("servers known", func(t *testing.T) {
		c := newRPCTestClient(t)
		c.servers.SetServers([]*servers.Server{{Addr: closedAddr(t)}})
		err := callRPC(t, c, 5*time.Second)
		must.ErrorIs(t, err, errNotRegistered)
	})

	t.Run("registered meanwhile", func(t *testing.T) {
		c := newRPCTestClient(t)
		c.config.RPCHoldTimeout = time.Minute
		errCh := make(chan error, 1)
		go func() { errCh <- c.waitRegistered() }()
		close(c.registeredCh)
		select {
		case err := <-errCh:
			must.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("still waiting after the registration")
		}
	})
}

// TestClient_ResolveToken_NotRegistered: the token resolution of a client
// endpoint (exec, fs, ...) on a client that never registered and has no
// reachable server fails instead of hanging.
func TestClient_ResolveToken_NotRegistered(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := TestClient(t, func(c *config.Config) {
		c.ACLEnabled = true
		c.RPCHoldTimeout = 100 * time.Millisecond
	})
	defer cleanup()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.ResolveToken("6c6e3d4a-1a1e-4a7b-8f5c-0d3c8c8e2a11")
		errCh <- err
	}()
	select {
	case err := <-errCh:
		must.ErrorIs(t, err, noServersErr)
	case <-time.After(10 * time.Second):
		t.Fatal("token resolution still waiting for the node registration")
	}
}

// TestRPC_ShutdownAfterFailedCall: a call that failed while the client shuts
// down returns the failure, not nil.
func TestRPC_ShutdownAfterFailedCall(t *testing.T) {
	ci.Parallel(t)
	c := newRPCTestClient(t)
	c.servers.SetServers([]*servers.Server{{Addr: closedAddr(t)}})
	close(c.registeredCh)
	close(c.shutdownCh)

	var reply structs.GenericResponse
	err := c.rpc("Node.UpdateAlloc", &structs.AllocUpdateRequest{}, &reply)
	must.ErrorIs(t, err, errShuttingDown)
}
