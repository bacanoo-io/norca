// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// forgetNodeRPC passes every RPC to the server, except Node.GetNode, which
// answers like a server that no longer has the node: no error, no node.
type forgetNodeRPC struct {
	config.RPCHandler
	forget atomic.Bool
}

func (f *forgetNodeRPC) RPC(method string, args, reply any) error {
	if f.forget.Load() && method == "Node.GetNode" {
		return nil
	}
	return f.RPCHandler.RPC(method, args, reply)
}

// Polling the drain status of a node the server no longer has ends the wait
// instead of dereferencing the missing node.
func TestClient_PollDrainStatus_ForgottenNode(t *testing.T) {
	ci.Parallel(t)

	srv, _, cleanupSRV := testServer(t, nil)
	defer cleanupSRV()
	testutil.WaitForLeader(t, srv.RPC)

	handler := &forgetNodeRPC{RPCHandler: srv}
	c1, cleanupC1 := TestClient(t, func(c *config.Config) {
		c.RPCHandler = handler
	})
	defer cleanupC1()

	select {
	case <-c1.registeredCh:
	case <-time.After(10 * time.Second):
		t.Fatal("client did not register")
	}
	handler.forget.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	must.NoError(t, c1.pollServerForDrainStatus(ctx, 10*time.Millisecond))
}
