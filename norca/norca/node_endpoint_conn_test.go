// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
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

// Node.Register and Node.GetClientAllocs keep a connection as the node's
// only for the node itself, as Node.UpdateStatus does (review finding SEC-3).
// Sent to a server of one region but addressed to another region, where the
// node does not exist, both returned without an error although nobody
// checked the node's secret (GetClientAllocs refuses an unknown node since
// review finding JOG-5); the first server then took the caller's connection
// for the newest connection of the node. The node itself still registers its
// connection through every path: at the server it reaches, and through a
// follower that forwards to the leader.
func TestClientEndpoint_RegisterAndGetClientAllocs_ConnOfNodeOnly(t *testing.T) {
	ci.Parallel(t)

	const otherRegion = "regionB"
	leader, root, cleanupLeader := TestACLServer(t, func(c *Config) {
		c.AuthoritativeRegion = "global"
	})
	defer cleanupLeader()
	testutil.WaitForLeader(t, leader.RPC)
	follower, cleanupFollower := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 0
		c.ACLEnabled = true
		c.AuthoritativeRegion = "global"
	})
	defer cleanupFollower()
	other, _, cleanupOther := TestACLServer(t, func(c *Config) {
		c.Region = otherRegion
		c.AuthoritativeRegion = "global"
		c.ReplicationBackoff = 20 * time.Millisecond
		c.ReplicationToken = root.SecretID
	})
	defer cleanupOther()
	TestJoin(t, leader, follower)
	TestJoin(t, leader, other)
	testutil.WaitForLeader(t, follower.RPC)
	testutil.WaitForLeader(t, other.RPC)
	must.True(t, leader.IsLeader())
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return len(leader.Regions()) == 2 && len(follower.Regions()) == 2
	}), wait.Timeout(20*time.Second), wait.Gap(50*time.Millisecond)))
	servers := []struct {
		name string
		srv  *Server
	}{{"leader", leader}, {"follower", follower}}

	// A node of the region "global", in the state of both of its servers, so
	// that either authenticates the node's secret.
	newNode := func(t *testing.T) *structs.Node {
		t.Helper()
		node := mock.Node()
		_, _, err := leader.raftApply(structs.NodeRegisterRequestType, &structs.NodeRegisterRequest{
			Node:         node.Copy(),
			WriteRequest: structs.WriteRequest{Region: "global"},
		})
		must.NoError(t, err)
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
			out, err := follower.State().NodeByID(nil, node.ID)
			return err == nil && out != nil
		}), wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))
		return node
	}
	// Every caller has a connection of its own.
	call := func(t *testing.T, s *Server, method string, req, reply any) error {
		t.Helper()
		codec := rpcClient(t, s)
		t.Cleanup(func() { codec.Close() })
		return msgpackrpc.CallWithCodec(codec, method, req, reply)
	}
	register := func(node *structs.Node, region, token string) (string, any, any) {
		return "Node.Register", &structs.NodeRegisterRequest{
			Node:         node,
			WriteRequest: structs.WriteRequest{Region: region, AuthToken: token},
		}, new(structs.NodeUpdateResponse)
	}
	clientAllocs := func(nodeID, secret, region, token string, stale bool) (string, any, any) {
		return "Node.GetClientAllocs", &structs.NodeSpecificRequest{
			NodeID:       nodeID,
			SecretID:     secret,
			QueryOptions: structs.QueryOptions{Region: region, AuthToken: token, AllowStale: stale},
		}, new(structs.NodeClientAllocsResponse)
	}
	// forged is a node with the ID of another one and a secret of its own.
	forged := func(id string) *structs.Node {
		node := mock.Node()
		node.ID = id
		return node
	}

	// A caller without the node's secret, through the other region.
	takeovers := []struct {
		name    string
		request func(victim *structs.Node) (string, any, any)
		// refused is the error of a region that does not know the node,
		// or "" when the call passes there.
		refused string
	}{
		{
			name: "GetClientAllocs addressed to the other region",
			request: func(victim *structs.Node) (string, any, any) {
				return clientAllocs(victim.ID, uuid.Generate(), otherRegion, "", false)
			},
			// an unknown node is answered like a wrong secret
			refused: "node secret ID does not match",
		},
		{
			name: "Register addressed to the other region",
			request: func(victim *structs.Node) (string, any, any) {
				return register(forged(victim.ID), otherRegion, "")
			},
		},
	}
	for _, s := range servers {
		for _, tc := range takeovers {
			t.Run(s.name+"/"+tc.name, func(t *testing.T) {
				victim := newNode(t)
				method, req, reply := tc.request(victim)
				// The other region does not know the node: where it has
				// nothing to refuse, that the call passes is what the
				// takeover needed.
				err := call(t, s.srv, method, req, reply)
				if tc.refused != "" {
					must.ErrorContains(t, err, tc.refused)
				} else {
					must.NoError(t, err)
				}
				_, ok := s.srv.getNodeConn(victim.ID)
				must.False(t, ok, must.Sprint("the connection of a caller without the node's secret became the connection of the node"))
			})
		}
	}

	// A caller that did not authenticate as the node, at the node's own
	// region: the same rule in the handlers.
	t.Run("leader/Register of a new node without its secret as the token", func(t *testing.T) {
		node := mock.Node()
		method, req, reply := register(node, "global", "")
		must.NoError(t, call(t, leader, method, req, reply))
		_, ok := leader.getNodeConn(node.ID)
		must.False(t, ok, must.Sprint("an unauthenticated registration kept its connection as the node's"))
	})
	t.Run("leader/GetClientAllocs without the node's secret as the token", func(t *testing.T) {
		node := newNode(t)
		method, req, reply := clientAllocs(node.ID, node.SecretID, "global", "", false)
		must.NoError(t, call(t, leader, method, req, reply))
		_, ok := leader.getNodeConn(node.ID)
		must.False(t, ok, must.Sprint("an unauthenticated caller kept its connection as the node's"))
	})

	// The node itself, as a client sends the requests: its secret is the
	// token.
	own := []struct {
		name    string
		srv     *Server
		request func(node *structs.Node) (string, any, any)
	}{
		{
			name: "leader/Register",
			srv:  leader,
			request: func(node *structs.Node) (string, any, any) {
				return register(node.Copy(), "global", node.SecretID)
			},
		},
		{
			name: "follower/Register forwarded to the leader",
			srv:  follower,
			request: func(node *structs.Node) (string, any, any) {
				return register(node.Copy(), "global", node.SecretID)
			},
		},
		{
			name: "leader/GetClientAllocs",
			srv:  leader,
			request: func(node *structs.Node) (string, any, any) {
				return clientAllocs(node.ID, node.SecretID, "global", node.SecretID, false)
			},
		},
		{
			name: "follower/GetClientAllocs forwarded to the leader",
			srv:  follower,
			request: func(node *structs.Node) (string, any, any) {
				return clientAllocs(node.ID, node.SecretID, "global", node.SecretID, false)
			},
		},
		{
			name: "follower/GetClientAllocs answered by the follower",
			srv:  follower,
			request: func(node *structs.Node) (string, any, any) {
				return clientAllocs(node.ID, node.SecretID, "global", node.SecretID, true)
			},
		},
	}
	for _, tc := range own {
		t.Run(tc.name, func(t *testing.T) {
			node := newNode(t)
			method, req, reply := tc.request(node)
			must.NoError(t, call(t, tc.srv, method, req, reply))
			conn, ok := tc.srv.getNodeConn(node.ID)
			must.True(t, ok, must.Sprint("the node's own connection is not registered"))
			must.Eq(t, node.ID, conn.Ctx.NodeID())
		})
	}
}
