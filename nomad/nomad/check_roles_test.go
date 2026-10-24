// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// TestServiceRegistration_GetService_ReadyFilter asserts the Ready state set by
// health check roles is stored and can be used to filter service instances.
func TestServiceRegistration_GetService_ReadyFilter(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	// two instances of the same service, one of them ready
	services := mock.ServiceRegistrations()
	second := services[0].Copy()
	second.ID += "_second"
	second.AllocID = services[1].AllocID
	services[0].Ready = true
	must.NoError(t, s.fsm.State().UpsertServiceRegistrations(
		structs.MsgTypeTestSetup, 10, []*structs.ServiceRegistration{services[0], second}))

	get := func(filter string) []*structs.ServiceRegistration {
		req := &structs.ServiceRegistrationByNameRequest{
			ServiceName: services[0].ServiceName,
			QueryOptions: structs.QueryOptions{
				Namespace: services[0].Namespace,
				Region:    DefaultRegion,
				Filter:    filter,
			},
		}
		var resp structs.ServiceRegistrationByNameResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, req, &resp))
		return resp.Services
	}

	must.Len(t, 2, get(""))

	ready := get("Ready == true")
	must.Len(t, 1, ready)
	must.Eq(t, services[0].ID, ready[0].ID)

	notReady := get("Ready == false")
	must.Len(t, 1, notReady)
	must.Eq(t, second.ID, notReady[0].ID)

	// the instance becomes ready: the upsert is not a no-op
	second = second.Copy()
	second.Ready = true
	must.NoError(t, s.fsm.State().UpsertServiceRegistrations(
		structs.MsgTypeTestSetup, 20, []*structs.ServiceRegistration{second}))

	ready = get("Ready == true")
	must.Len(t, 2, ready)
	for _, reg := range ready {
		if reg.ID == second.ID {
			must.Eq(t, uint64(20), reg.ModifyIndex)
		}
	}
}

func TestServiceRegistration_nodeReportsServiceReadiness(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name  string
		attrs map[string]string
		exp   bool
	}{
		{name: "upstream nomad", attrs: map[string]string{"nomad.version": "1.6.5"}, exp: false},
		{name: "older norca", attrs: map[string]string{"norca.version": "0.0.9"}, exp: false},
		{name: "unparsable", attrs: map[string]string{"norca.version": "norca"}, exp: false},
		{name: "minimum", attrs: map[string]string{"norca.version": "0.1.0"}, exp: true},
		{name: "prerelease", attrs: map[string]string{"norca.version": "0.1.0-dev"}, exp: true},
		{name: "newer", attrs: map[string]string{"norca.version": "0.2.0"}, exp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := mock.Node()
			node.Attributes = tc.attrs
			must.Eq(t, tc.exp, nodeReportsServiceReadiness(node))
		})
	}
}

// TestServiceRegistration_Upsert_ReadyCompat asserts that registrations from
// clients without the Ready flag are stored as ready, while the Ready state
// sent by Norca clients is kept as is.
func TestServiceRegistration_Upsert_ReadyCompat(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	legacyNode := mock.Node()
	delete(legacyNode.Attributes, "norca.version")
	norcaNode := mock.Node()
	norcaNode.Attributes["norca.version"] = "0.1.0"
	must.NoError(t, s.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 10, legacyNode))
	must.NoError(t, s.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 11, norcaNode))

	upsert := func(node *structs.Node, services []*structs.ServiceRegistration) {
		req := &structs.ServiceRegistrationUpsertRequest{
			Services: services,
			WriteRequest: structs.WriteRequest{
				Region:    DefaultRegion,
				AuthToken: node.SecretID,
			},
		}
		var resp structs.ServiceRegistrationUpsertResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationUpsertRPCMethod, req, &resp))
	}
	stored := func(reg *structs.ServiceRegistration) *structs.ServiceRegistration {
		out, err := s.fsm.State().GetServiceRegistrationByID(nil, reg.Namespace, reg.ID)
		must.NoError(t, err)
		must.NotNil(t, out)
		return out
	}

	// a legacy client sends Ready = false for a healthy instance
	legacy := mock.ServiceRegistrations()
	legacy[0].ID += "_legacy"
	legacy[0].NodeID = legacyNode.ID
	legacy[0].Ready = false
	upsert(legacyNode, legacy[:1])
	must.True(t, stored(legacy[0]).Ready)

	// a Norca client reports both states itself
	norca := mock.ServiceRegistrations()
	norca[0].NodeID = norcaNode.ID
	norca[0].Ready = false
	norca[1].Namespace = norca[0].Namespace
	norca[1].NodeID = norcaNode.ID
	norca[1].Ready = true
	upsert(norcaNode, norca)
	must.False(t, stored(norca[0]).Ready)
	must.True(t, stored(norca[1]).Ready)
}
