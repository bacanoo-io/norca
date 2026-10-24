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
