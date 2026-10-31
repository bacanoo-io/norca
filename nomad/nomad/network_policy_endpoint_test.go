// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func testNetworkPolicy(namespace, name string, selector map[string]string) *structs.NetworkPolicy {
	return &structs.NetworkPolicy{
		Name:      name,
		Namespace: namespace,
		Selector:  selector,
		Egress: []*structs.NetworkPolicyRule{{
			Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"10.20.0.0/16"}}},
			Ports: []string{"443/tcp"},
		}},
	}
}

func TestNetworkPolicyEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	// The namespace must exist.
	upsertReq := &structs.NetworkPolicyUpsertRequest{
		NetworkPolicies: []*structs.NetworkPolicy{testNetworkPolicy("prod", "a", nil)},
		WriteRequest:    structs.WriteRequest{Region: "global"},
	}
	var resp structs.GenericResponse
	err := msgpackrpc.CallWithCodec(codec, "NetworkPolicy.UpsertNetworkPolicies", upsertReq, &resp)
	must.ErrorContains(t, err, `namespace "prod" does not exist`)

	must.NoError(t, s.State().UpsertNamespaces(900, []*structs.Namespace{{Name: "prod"}}))

	// The namespace defaults to the namespace of the request.
	upsertReq.NetworkPolicies = []*structs.NetworkPolicy{
		testNetworkPolicy("prod", "a", nil),
		testNetworkPolicy("", "b", map[string]string{"app": "db"}),
	}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.UpsertNetworkPolicies", upsertReq, &resp))
	must.NonZero(t, resp.Index)

	// Invalid policies are rejected.
	bad := testNetworkPolicy("prod", "bad", nil)
	bad.Egress[0].Ports = []string{"99999"}
	upsertReq.NetworkPolicies = []*structs.NetworkPolicy{bad}
	err = msgpackrpc.CallWithCodec(codec, "NetworkPolicy.UpsertNetworkPolicies", upsertReq, &resp)
	must.ErrorContains(t, err, "invalid network policy")

	list := func(ns string) []*structs.NetworkPolicy {
		req := &structs.NetworkPolicyListRequest{QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns}}
		var out structs.NetworkPolicyListResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.List", req, &out))
		return out.NetworkPolicies
	}
	must.Len(t, 1, list("prod"))
	must.Len(t, 1, list("default"))
	must.Len(t, 2, list("*"))

	getReq := &structs.NetworkPolicySpecificRequest{Name: "a", QueryOptions: structs.QueryOptions{Region: "global", Namespace: "prod"}}
	var getResp structs.SingleNetworkPolicyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetNetworkPolicy", getReq, &getResp))
	must.NotNil(t, getResp.NetworkPolicy)
	must.Eq(t, structs.NetworkPolicyEnforcementRequired, getResp.NetworkPolicy.Enforcement)

	// Delete; deleting a missing policy is a not found error.
	delReq := &structs.NetworkPolicyDeleteRequest{Names: []string{"a"}, WriteRequest: structs.WriteRequest{Region: "global", Namespace: "prod"}}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.DeleteNetworkPolicies", delReq, &resp))
	err = msgpackrpc.CallWithCodec(codec, "NetworkPolicy.DeleteNetworkPolicies", delReq, &resp)
	must.ErrorContains(t, err, "not found")
	must.Len(t, 0, list("prod"))
}

func TestNetworkPolicyEndpoint_AdminCRUD(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	upsertReq := &structs.AdminNetworkPolicyUpsertRequest{
		AdminNetworkPolicies: []*structs.AdminNetworkPolicy{
			{Name: "late", Priority: 20, Egress: []*structs.NetworkPolicyRule{{Action: "pass"}}},
			{Name: "early", Priority: 10, Egress: []*structs.NetworkPolicyRule{{Action: "deny",
				Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"169.254.169.254/32"}}}}}},
		},
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var resp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.UpsertAdminNetworkPolicies", upsertReq, &resp))

	listReq := &structs.AdminNetworkPolicyListRequest{QueryOptions: structs.QueryOptions{Region: "global"}}
	var listResp structs.AdminNetworkPolicyListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.ListAdminNetworkPolicies", listReq, &listResp))
	must.Len(t, 2, listResp.AdminNetworkPolicies)
	must.Eq(t, "early", listResp.AdminNetworkPolicies[0].Name)

	// Rules without an action are invalid in admin policies.
	upsertReq.AdminNetworkPolicies = []*structs.AdminNetworkPolicy{{Name: "x", Egress: []*structs.NetworkPolicyRule{{}}}}
	must.ErrorContains(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.UpsertAdminNetworkPolicies", upsertReq, &resp), "invalid action")

	delReq := &structs.AdminNetworkPolicyDeleteRequest{Names: []string{"late"}, WriteRequest: structs.WriteRequest{Region: "global"}}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.DeleteAdminNetworkPolicies", delReq, &resp))
	getReq := &structs.AdminNetworkPolicySpecificRequest{Name: "late", QueryOptions: structs.QueryOptions{Region: "global"}}
	var getResp structs.SingleAdminNetworkPolicyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAdminNetworkPolicy", getReq, &getResp))
	must.Nil(t, getResp.AdminNetworkPolicy)
}

func TestNetworkPolicyEndpoint_ACL(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	store := s.State()
	must.NoError(t, store.UpsertNamespaces(900, []*structs.Namespace{{Name: "prod"}}))

	writer := mock.CreatePolicyAndToken(t, store, 1001, "np-write",
		`namespace "prod" { network_policy { policy = "write" } }`)
	reader := mock.CreatePolicyAndToken(t, store, 1002, "np-read",
		`namespace "prod" { policy = "read" }`)
	jobWriter := mock.CreatePolicyAndToken(t, store, 1003, "job-write",
		`namespace "prod" { policy = "write" }`)
	operator := mock.CreatePolicyAndToken(t, store, 1004, "operator", `operator { policy = "write" }`)

	upsert := func(token string) error {
		req := &structs.NetworkPolicyUpsertRequest{
			NetworkPolicies: []*structs.NetworkPolicy{testNetworkPolicy("prod", "a", nil)},
			WriteRequest:    structs.WriteRequest{Region: "global", AuthToken: token},
		}
		var resp structs.GenericResponse
		return msgpackrpc.CallWithCodec(codec, "NetworkPolicy.UpsertNetworkPolicies", req, &resp)
	}
	must.EqError(t, upsert(""), structs.ErrPermissionDenied.Error())
	must.EqError(t, upsert(reader.SecretID), structs.ErrPermissionDenied.Error())
	// The namespace write policy does not include write-network-policy.
	must.EqError(t, upsert(jobWriter.SecretID), structs.ErrPermissionDenied.Error())
	must.NoError(t, upsert(writer.SecretID))

	// The namespace read policy includes read-network-policy.
	listReq := &structs.NetworkPolicyListRequest{QueryOptions: structs.QueryOptions{Region: "global", Namespace: "prod", AuthToken: reader.SecretID}}
	var listResp structs.NetworkPolicyListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.List", listReq, &listResp))
	must.Len(t, 1, listResp.NetworkPolicies)

	// The wildcard list filters by namespace.
	listReq.Namespace = "*"
	listReq.AuthToken = operator.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.List", listReq, &listResp))
	must.Len(t, 0, listResp.NetworkPolicies)
	listReq.AuthToken = root.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.List", listReq, &listResp))
	must.Len(t, 1, listResp.NetworkPolicies)

	// Admin policies need operator write.
	adminUpsert := func(token string) error {
		req := &structs.AdminNetworkPolicyUpsertRequest{
			AdminNetworkPolicies: []*structs.AdminNetworkPolicy{{Name: "x", Egress: []*structs.NetworkPolicyRule{{Action: "pass"}}}},
			WriteRequest:         structs.WriteRequest{Region: "global", AuthToken: token},
		}
		var resp structs.GenericResponse
		return msgpackrpc.CallWithCodec(codec, "NetworkPolicy.UpsertAdminNetworkPolicies", req, &resp)
	}
	must.EqError(t, adminUpsert(writer.SecretID), structs.ErrPermissionDenied.Error())
	must.NoError(t, adminUpsert(operator.SecretID))

	adminList := &structs.AdminNetworkPolicyListRequest{QueryOptions: structs.QueryOptions{Region: "global", AuthToken: reader.SecretID}}
	var adminResp structs.AdminNetworkPolicyListResponse
	must.EqError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.ListAdminNetworkPolicies", adminList, &adminResp),
		structs.ErrPermissionDenied.Error())

	// ACL capability helpers.
	aclObj, err := s.ResolveACLForToken(reader)
	must.NoError(t, err)
	must.True(t, aclObj.AllowNsOp("prod", acl.NamespaceCapabilityReadNetworkPolicy))
	must.False(t, aclObj.AllowNsOp("prod", acl.NamespaceCapabilityWriteNetworkPolicy))
}

func TestNetworkPolicyEndpoint_AllocRulesAndStatus(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	store := s.State()

	node := mock.Node()
	node.Attributes[structs.NetworkPolicyNftablesAttr] = "true"
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1000, node))
	other := mock.Node()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1001, other))

	newAlloc := func(nodeID, app, addr string) *structs.Allocation {
		a := mock.Alloc()
		a.NodeID = nodeID
		a.Job.Meta = map[string]string{"app": app}
		a.Job.TaskGroups[0].Networks = structs.Networks{{Mode: "bridge"}}
		a.NetworkStatus = &structs.AllocNetworkStatus{Address: addr}
		a.ClientStatus = structs.AllocClientStatusRunning
		return a
	}
	db := newAlloc(node.ID, "postgres", "172.26.64.2")
	api := newAlloc(node.ID, "api", "172.26.64.3")
	remoteAPI := newAlloc(other.ID, "api", "172.26.64.4")
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1002, []*structs.Allocation{db, api, remoteAPI}))

	policy := &structs.NetworkPolicy{
		Name:      "db",
		Namespace: "default",
		Selector:  map[string]string{"app": "postgres"},
		Ingress: []*structs.NetworkPolicyRule{{
			Peers: []*structs.NetworkPolicyPeer{{Selector: map[string]string{"app": "api"}}},
			Ports: []string{"5432/tcp"},
		}},
		Enforcement: structs.NetworkPolicyEnforcementRequired,
	}
	must.NoError(t, store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 1003, []*structs.NetworkPolicy{policy}))

	// The client of the node gets the rules with the local peer only.
	req := &structs.NetworkPolicyAllocRequest{
		AllocID:      db.ID,
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: node.SecretID},
	}
	var resp structs.NetworkPolicyAllocResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAllocRules", req, &resp))
	must.Len(t, 1, resp.Rules.Policies)
	must.Len(t, 1, resp.Rules.Peers)
	must.Eq(t, api.ID, resp.Rules.Peers[0].ID)
	must.Eq(t, "172.26.64.3", resp.Rules.Peers[0].Address)

	// Another node may not read the rules of the allocation.
	req.AuthToken = other.SecretID
	must.EqError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAllocRules", req, &resp), structs.ErrPermissionDenied.Error())

	// white room bugfix: a token without the permissions gets the same
	// answer for an existing and a missing allocation; one with them gets
	// an empty rule set for a missing allocation.
	outsider := mock.CreatePolicyAndToken(t, store, 1004, "outsider", `node { policy = "read" }`)
	missing := "8f1b3c2a-0000-4000-8000-000000000001"
	for _, id := range []string{db.ID, missing} {
		probe := &structs.NetworkPolicyAllocRequest{AllocID: id,
			QueryOptions: structs.QueryOptions{Region: "global", AuthToken: outsider.SecretID}}
		must.EqError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAllocRules", probe, &resp), structs.ErrPermissionDenied.Error())
	}
	probe := &structs.NetworkPolicyAllocRequest{AllocID: missing,
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID}}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAllocRules", probe, &resp))
	must.True(t, resp.Rules.Empty())

	// An allocation no policy selects gets an empty rule set.
	req.AllocID = api.ID
	req.AuthToken = node.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAllocRules", req, &resp))
	must.True(t, resp.Rules.Empty())

	// A blocking query returns once a policy changes.
	req.AllocID = db.ID
	req.MinQueryIndex = resp.Index
	req.MaxQueryTime = 10 * time.Second
	go func() {
		time.Sleep(100 * time.Millisecond)
		p := policy.Copy()
		p.Ingress[0].Ports = []string{"5433/tcp"}
		store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 2000, []*structs.NetworkPolicy{p})
	}()
	start := time.Now()
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.GetAllocRules", req, &resp))
	must.Less(t, 5*time.Second, time.Since(start))
	must.Eq(t, uint64(2000), resp.Index)
	must.Eq(t, []string{"5433/tcp"}, resp.Rules.Policies[0].Ingress[0].Ports)

	// Status lists the selected allocation and its enforceability.
	statusReq := &structs.NetworkPolicyStatusRequest{
		Name:         "db",
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID},
	}
	var statusResp structs.NetworkPolicyStatusResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.Status", statusReq, &statusResp))
	must.Len(t, 1, statusResp.Allocations)
	must.Eq(t, db.ID, statusResp.Allocations[0].ID)
	must.True(t, statusResp.Allocations[0].Enforceable)
	must.Eq(t, "bridge", statusResp.Allocations[0].NetworkMode)

	statusReq.Name = "missing"
	must.ErrorContains(t, msgpackrpc.CallWithCodec(codec, "NetworkPolicy.Status", statusReq, &statusResp), "not found")
}

func TestJobNetworkPolicyHook_Warnings(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	testutil.WaitForLeader(t, s.RPC)

	required := testNetworkPolicy("default", "req", map[string]string{"app": "web"})
	required.Enforcement = structs.NetworkPolicyEnforcementRequired
	best := testNetworkPolicy("default", "best", nil)
	best.Enforcement = structs.NetworkPolicyEnforcementBestEffort
	must.NoError(t, s.State().UpsertNetworkPolicies(structs.MsgTypeTestSetup, 100, []*structs.NetworkPolicy{required, best}))

	job := mock.Job()
	job.Meta = map[string]string{"app": "web"}
	job.TaskGroups[0].Networks = nil // host mode

	warnings, err := jobNetworkPolicyHook{srv: s}.Validate(job)
	must.NoError(t, err)
	must.Len(t, 2, warnings)
	must.StrContains(t, warnings[0].Error(), "will not be placed")
	must.StrContains(t, warnings[1].Error(), "no effect")

	// Bridge mode can enforce the rules: no warning.
	job.TaskGroups[0].Networks = structs.Networks{{Mode: "bridge"}}
	warnings, err = jobNetworkPolicyHook{srv: s}.Validate(job)
	must.NoError(t, err)
	must.Len(t, 0, warnings)
}

func TestFSM_SnapshotRestore_NetworkPolicies(t *testing.T) {
	ci.Parallel(t)

	fsm := testFSM(t)
	store := fsm.State()
	must.NoError(t, store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 10,
		[]*structs.NetworkPolicy{testNetworkPolicy("default", "a", map[string]string{"app": "db"})}))
	must.NoError(t, store.UpsertAdminNetworkPolicies(structs.MsgTypeTestSetup, 11,
		[]*structs.AdminNetworkPolicy{{Name: "adm", Priority: 3, Egress: []*structs.NetworkPolicyRule{{Action: "deny"}}}}))

	fsm2 := testSnapshotRestore(t, fsm)
	store2 := fsm2.State()
	p, err := store2.NetworkPolicyByName(nil, "default", "a")
	must.NoError(t, err)
	must.NotNil(t, p)
	must.Eq(t, "db", p.Selector["app"])
	a, err := store2.AdminNetworkPolicyByName(nil, "adm")
	must.NoError(t, err)
	must.NotNil(t, a)
	must.Eq(t, 3, a.Priority)
}

func TestFSM_NetworkPolicyApply(t *testing.T) {
	ci.Parallel(t)

	fsm := testFSM(t)
	req := structs.NetworkPolicyUpsertRequest{
		NetworkPolicies: []*structs.NetworkPolicy{testNetworkPolicy("default", "a", nil)},
	}
	buf, err := structs.Encode(structs.NetworkPolicyUpsertRequestType, req)
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	del := structs.NetworkPolicyDeleteRequest{Names: []string{"a"}, WriteRequest: structs.WriteRequest{Namespace: "default"}}
	buf, err = structs.Encode(structs.NetworkPolicyDeleteRequestType, del)
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))
	p, err := fsm.State().NetworkPolicyByName(nil, "default", "a")
	must.NoError(t, err)
	must.Nil(t, p)

	admin := structs.AdminNetworkPolicyUpsertRequest{
		AdminNetworkPolicies: []*structs.AdminNetworkPolicy{{Name: "adm"}},
	}
	buf, err = structs.Encode(structs.AdminNetworkPolicyUpsertRequestType|structs.IgnoreUnknownTypeFlag, admin)
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))
	a, err := fsm.State().AdminNetworkPolicyByName(nil, "adm")
	must.NoError(t, err)
	must.NotNil(t, a)
}
