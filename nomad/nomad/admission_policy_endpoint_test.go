// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func testAdmissionPolicy(name string, namespaces []string, action, expr string) *structs.AdmissionPolicy {
	p := &structs.AdmissionPolicy{
		Name:   name,
		Action: action,
		Rules: []*structs.AdmissionPolicyRule{{
			Expression: expr,
			Message:    name + " violated",
		}},
	}
	if namespaces != nil {
		p.Match = &structs.AdmissionPolicyMatch{Namespaces: namespaces}
	}
	return p
}

func TestAdmissionPolicyEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	// Upsert two policies.
	upsertReq := &structs.AdmissionPolicyUpsertRequest{
		AdmissionPolicies: []*structs.AdmissionPolicy{
			testAdmissionPolicy("p1", nil, "", `job.Priority < 90`),
			testAdmissionPolicy("p2", []string{"prod"}, "warn", `has(job.Meta.team)`),
		},
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var upsertResp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.UpsertAdmissionPolicies", upsertReq, &upsertResp))
	must.NonZero(t, upsertResp.Index)

	// List them.
	listReq := &structs.AdmissionPolicyListRequest{QueryOptions: structs.QueryOptions{Region: "global"}}
	var listResp structs.AdmissionPolicyListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.List", listReq, &listResp))
	must.Len(t, 2, listResp.AdmissionPolicies)
	must.Eq(t, upsertResp.Index, listResp.Index)

	// Prefix list.
	listReq.Prefix = "p2"
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.List", listReq, &listResp))
	must.Len(t, 1, listResp.AdmissionPolicies)

	// Get one; the default action is deny.
	getReq := &structs.AdmissionPolicySpecificRequest{Name: "p1", QueryOptions: structs.QueryOptions{Region: "global"}}
	var getResp structs.SingleAdmissionPolicyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.GetAdmissionPolicy", getReq, &getResp))
	must.NotNil(t, getResp.AdmissionPolicy)
	must.Eq(t, structs.AdmissionPolicyActionDeny, getResp.AdmissionPolicy.Action)
	must.Eq(t, upsertResp.Index, getResp.AdmissionPolicy.CreateIndex)

	// Update keeps the create index.
	upsertReq.AdmissionPolicies = []*structs.AdmissionPolicy{testAdmissionPolicy("p1", nil, "warn", `true`)}
	var updateResp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.UpsertAdmissionPolicies", upsertReq, &updateResp))
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.GetAdmissionPolicy", getReq, &getResp))
	must.Eq(t, upsertResp.Index, getResp.AdmissionPolicy.CreateIndex)
	must.Eq(t, updateResp.Index, getResp.AdmissionPolicy.ModifyIndex)
	must.Eq(t, "warn", getResp.AdmissionPolicy.Action)

	// Delete one.
	delReq := &structs.AdmissionPolicyDeleteRequest{Names: []string{"p1"}, WriteRequest: structs.WriteRequest{Region: "global"}}
	var delResp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.DeleteAdmissionPolicies", delReq, &delResp))
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.GetAdmissionPolicy", getReq, &getResp))
	must.Nil(t, getResp.AdmissionPolicy)

	// Deleting a missing policy fails.
	err := msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.DeleteAdmissionPolicies", delReq, &delResp)
	must.ErrorContains(t, err, "not found")
}

func TestAdmissionPolicyEndpoint_Upsert_Invalid(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	cases := map[string]*structs.AdmissionPolicy{
		"syntax":      testAdmissionPolicy("bad", nil, "", `job.ID ==`),
		"undeclared":  testAdmissionPolicy("bad", nil, "", `nope.ID == 'x'`),
		"not bool":    testAdmissionPolicy("bad", nil, "", `'job' + 'x'`),
		"bad action":  testAdmissionPolicy("bad", nil, "audit", `true`),
		"bad name":    testAdmissionPolicy("bad name", nil, "", `true`),
		"no rules":    {Name: "bad"},
		"empty match": testAdmissionPolicy("bad", []string{""}, "", `true`),
	}
	for name, policy := range cases {
		t.Run(name, func(t *testing.T) {
			req := &structs.AdmissionPolicyUpsertRequest{
				AdmissionPolicies: []*structs.AdmissionPolicy{policy},
				WriteRequest:      structs.WriteRequest{Region: "global"},
			}
			var resp structs.GenericResponse
			err := msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.UpsertAdmissionPolicies", req, &resp)
			must.Error(t, err)
		})
	}

	// Nothing was stored.
	policy, err := s.State().AdmissionPolicyByName(nil, "bad")
	must.NoError(t, err)
	must.Nil(t, policy)
}

func TestAdmissionPolicyEndpoint_ACL(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	readToken := mock.CreatePolicyAndToken(t, s.State(), 1001, "admission-read", `admission { policy = "read" }`)
	writeToken := mock.CreatePolicyAndToken(t, s.State(), 1002, "admission-write", `admission { policy = "write" }`)
	otherToken := mock.CreatePolicyAndToken(t, s.State(), 1003, "operator", `operator { policy = "write" }`)

	upsert := func(token string) error {
		req := &structs.AdmissionPolicyUpsertRequest{
			AdmissionPolicies: []*structs.AdmissionPolicy{testAdmissionPolicy("p1", nil, "", `true`)},
			WriteRequest:      structs.WriteRequest{Region: "global", AuthToken: token},
		}
		var resp structs.GenericResponse
		return msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.UpsertAdmissionPolicies", req, &resp)
	}
	list := func(token string) error {
		req := &structs.AdmissionPolicyListRequest{QueryOptions: structs.QueryOptions{Region: "global", AuthToken: token}}
		var resp structs.AdmissionPolicyListResponse
		return msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.List", req, &resp)
	}
	get := func(token string) error {
		req := &structs.AdmissionPolicySpecificRequest{Name: "p1", QueryOptions: structs.QueryOptions{Region: "global", AuthToken: token}}
		var resp structs.SingleAdmissionPolicyResponse
		return msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.GetAdmissionPolicy", req, &resp)
	}
	del := func(token string) error {
		req := &structs.AdmissionPolicyDeleteRequest{Names: []string{"p1"}, WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token}}
		var resp structs.GenericResponse
		return msgpackrpc.CallWithCodec(codec, "AdmissionPolicy.DeleteAdmissionPolicies", req, &resp)
	}

	for _, token := range []string{"", otherToken.SecretID, readToken.SecretID} {
		must.EqError(t, upsert(token), structs.ErrPermissionDenied.Error())
	}
	must.NoError(t, upsert(writeToken.SecretID))
	must.NoError(t, upsert(root.SecretID))

	for _, token := range []string{"", otherToken.SecretID} {
		must.EqError(t, list(token), structs.ErrPermissionDenied.Error())
		must.EqError(t, get(token), structs.ErrPermissionDenied.Error())
	}
	for _, token := range []string{readToken.SecretID, writeToken.SecretID, root.SecretID} {
		must.NoError(t, list(token))
		must.NoError(t, get(token))
	}

	must.EqError(t, del(readToken.SecretID), structs.ErrPermissionDenied.Error())
	must.NoError(t, del(writeToken.SecretID))
}

func TestAdmissionPolicy_SnapshotRestore(t *testing.T) {
	ci.Parallel(t)

	fsm := testFSM(t)
	policy := testAdmissionPolicy("p1", []string{"prod"}, "warn", `true`)
	must.NoError(t, fsm.State().UpsertAdmissionPolicies(structs.MsgTypeTestSetup, 10, []*structs.AdmissionPolicy{policy}))

	fsm2 := testSnapshotRestore(t, fsm)
	out, err := fsm2.State().AdmissionPolicyByName(nil, "p1")
	must.NoError(t, err)
	must.Eq(t, policy, out)
}

func TestAdmissionPolicy_FSMApply(t *testing.T) {
	ci.Parallel(t)

	fsm := testFSM(t)
	req := structs.AdmissionPolicyUpsertRequest{
		AdmissionPolicies: []*structs.AdmissionPolicy{testAdmissionPolicy("p1", nil, "deny", `true`)},
	}
	buf, err := structs.Encode(structs.AdmissionPolicyUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	out, err := fsm.State().AdmissionPolicyByName(nil, "p1")
	must.NoError(t, err)
	must.NotNil(t, out)

	delReq := structs.AdmissionPolicyDeleteRequest{Names: []string{"p1"}}
	buf, err = structs.Encode(structs.AdmissionPolicyDeleteRequestType|structs.IgnoreUnknownTypeFlag, delReq)
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	out, err = fsm.State().AdmissionPolicyByName(nil, "p1")
	must.NoError(t, err)
	must.Nil(t, out)
}
