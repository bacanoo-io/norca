// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// TestNetworkPolicyEndpoint_AllocRulesOtherNamespaces checks what a caller
// that is not the node of the allocation gets from GetAllocRules: the peers
// of the namespaces whose jobs it may read, and the admin policies only
// with operator read. A policy with a peer of every namespace must not list
// the allocations of the other namespaces of the node, with their meta and
// addresses, to someone with rights in one namespace.
func TestNetworkPolicyEndpoint_AllocRulesOtherNamespaces(t *testing.T) {
	ci.Parallel(t)

	s, _, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	store := s.State()

	nsA, nsB := mock.Namespace(), mock.Namespace()
	nsA.Name, nsB.Name = "team-a", "team-b"
	must.NoError(t, store.UpsertNamespaces(999, []*structs.Namespace{nsA, nsB}))
	node := mock.Node()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	newAlloc := func(namespace, app, addr string) *structs.Allocation {
		a := mock.Alloc()
		a.NodeID = node.ID
		a.Namespace = namespace
		a.Job.Namespace = namespace
		a.Job.Meta = map[string]string{"app": app}
		a.Job.TaskGroups[0].Networks = structs.Networks{{Mode: "bridge"}}
		a.NetworkStatus = &structs.AllocNetworkStatus{Address: addr}
		a.ClientStatus = structs.AllocClientStatusRunning
		return a
	}
	own := newAlloc("team-a", "probe", "172.26.64.2")
	neighbour := newAlloc("team-a", "web", "172.26.64.3")
	foreign := newAlloc("team-b", "billing", "172.26.64.4")
	foreign.Job.Meta["db_url"] = "postgres://billing.internal/ledger"
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1002, []*structs.Allocation{own, neighbour, foreign}))

	// What a holder of write-network-policy in team-a can write: a peer of
	// every namespace with an empty selector.
	policy := &structs.NetworkPolicy{
		Name:      "probe",
		Namespace: "team-a",
		Selector:  map[string]string{"app": "probe"},
		Egress: []*structs.NetworkPolicyRule{{
			Peers: []*structs.NetworkPolicyPeer{{Namespace: "*"}},
		}},
	}
	must.NoError(t, store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 1003, []*structs.NetworkPolicy{policy}))
	admin := &structs.AdminNetworkPolicy{
		Name:        "cluster-baseline",
		Description: "operators only",
		Egress:      []*structs.NetworkPolicyRule{{Action: "pass"}},
	}
	must.NoError(t, store.UpsertAdminNetworkPolicies(structs.MsgTypeTestSetup, 1004, []*structs.AdminNetworkPolicy{admin}))

	teamA := mock.CreatePolicyAndToken(t, store, 1010, "team-a", `
namespace "team-a" { capabilities = ["read-job", "read-network-policy", "write-network-policy"] }`)
	both := mock.CreatePolicyAndToken(t, store, 1012, "both-teams", `
namespace "team-a" { capabilities = ["read-job", "read-network-policy"] }
namespace "team-b" { capabilities = ["read-job"] }
operator { policy = "read" }`)

	rules := func(token string) *structs.NetworkPolicyAllocRules {
		req := &structs.NetworkPolicyAllocRequest{
			AllocID:      own.ID,
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: "team-a", AuthToken: token},
		}
		var resp structs.NetworkPolicyAllocResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAllocRules", req, &resp))
		return resp.Rules
	}
	peerIDs := func(r *structs.NetworkPolicyAllocRules) []string {
		ids := []string{}
		for _, p := range r.Peers {
			ids = append(ids, p.ID+" "+p.Namespace)
		}
		return ids
	}
	all := []string{neighbour.ID + " team-a", foreign.ID + " team-b"}
	if foreign.ID < neighbour.ID {
		all[0], all[1] = all[1], all[0]
	}

	// Rights in team-a only: its own namespace's peer and policy, nothing
	// of team-b and no admin policy.
	got := rules(teamA.SecretID)
	must.Len(t, 1, got.Policies)
	must.Eq(t, []string{neighbour.ID + " team-a"}, peerIDs(got),
		must.Sprint("peers shown to a token with rights in team-a only"))
	must.SliceEmpty(t, got.AdminPolicies, must.Sprint("admin policies shown without operator read"))

	// read-job in both namespaces and operator read: everything.
	got = rules(both.SecretID)
	must.Eq(t, all, peerIDs(got))
	must.Len(t, 1, got.AdminPolicies)
	must.Eq(t, "cluster-baseline", got.AdminPolicies[0].Name)

	// The node of the allocation enforces the rules: everything.
	got = rules(node.SecretID)
	must.Eq(t, all, peerIDs(got))
	must.Len(t, 1, got.AdminPolicies)
	for _, p := range got.Peers {
		if p.ID == foreign.ID {
			must.Eq(t, "postgres://billing.internal/ledger", p.Meta["db_url"])
		}
	}
}
