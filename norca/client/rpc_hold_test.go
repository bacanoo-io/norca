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
