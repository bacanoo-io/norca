// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// A caller with node write may set the status of a node, but its connection
// must not become the connection of the node (review finding SRV-2): the
// servers send alloc exec, fs and log streams over it, with the token of the
// user who asked. Only the node itself, by its secret ID, registers one.
func TestClientEndpoint_UpdateStatus_ConnOfNodeOnly(t *testing.T) {
	ci.Parallel(t)

	leader, root, cleanupLeader := TestACLServer(t, nil)
	defer cleanupLeader()
	testutil.WaitForLeader(t, leader.RPC)
	follower, cleanupFollower := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 0
		c.ACLEnabled = true
	})
	defer cleanupFollower()
	TestJoin(t, leader, follower)
	testutil.WaitForLeader(t, follower.RPC)
	must.True(t, leader.IsLeader())

	// The tokens are the leader's; the follower forwards. The nodes are in
	// the state of both, so that either authenticates a node secret.
	nodeWriter := mock.CreatePolicyAndToken(t, leader.State(), 1001, "node-write", mock.NodePolicy(acl.PolicyWrite))
	nodes := map[*Server]*structs.Node{leader: mock.Node(), follower: mock.Node()}
	index := uint64(1010)
	for _, node := range nodes {
		for _, s := range []*Server{leader, follower} {
			must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, index, node.Copy()))
		}
		index++
	}

	for name, s := range map[string]*Server{"leader": leader, "follower": follower} {
		node := nodes[s]
		update := func(token string) error {
			// A connection of its own for every caller.
			codec := rpcClient(t, s)
			t.Cleanup(func() { codec.Close() })
			req := &structs.NodeUpdateStatusRequest{
				NodeID:       node.ID,
				Status:       structs.NodeStatusReady,
				WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token},
			}
			return msgpackrpc.CallWithCodec(codec, "Node.UpdateStatus", req, new(structs.NodeUpdateResponse))
		}

		// Node write, and a management token: the status call passes, the
		// connection is not the node's.
		for _, token := range []*structs.ACLToken{nodeWriter, root} {
			must.NoError(t, update(token.SecretID), must.Sprint(name))
			_, ok := s.getNodeConn(node.ID)
			must.False(t, ok, must.Sprintf("%s: the connection of token %q became the connection of the node", name, token.Name))
		}

		// The node itself.
		must.NoError(t, update(node.SecretID), must.Sprint(name))
		_, ok := s.getNodeConn(node.ID)
		must.True(t, ok, must.Sprintf("%s: the node's own connection is not registered", name))

		// A later call with a token does not take it over either.
		before, _ := s.getNodeConn(node.ID)
		must.NoError(t, update(nodeWriter.SecretID), must.Sprint(name))
		after, ok := s.getNodeConn(node.ID)
		must.True(t, ok)
		must.Eq(t, before.Ctx, after.Ctx, must.Sprintf("%s: a token call replaced the node's connection", name))
	}
}
