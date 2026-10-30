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

func TestJobEndpoint_Register_ConsulProviderWarning(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	register := func(provider string) string {
		t.Helper()
		job := mock.Job()
		for _, s := range job.TaskGroups[0].Tasks[0].Services {
			s.Provider = provider
			s.Checks = nil // script checks need Consul
		}
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
		return resp.Warnings
	}

	// A region without nodes: no warning.
	must.StrNotContains(t, register(structs.ServiceProviderConsul), "Consul agent")

	// Without a provider the services go to the native catalog.
	node := mock.Node()
	delete(node.Attributes, "consul.version")
	must.NoError(t, s1.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 1000, node))
	must.StrNotContains(t, register(""), "Consul agent")

	// Consul without a Consul agent in the region: a warning.
	warnings := register(structs.ServiceProviderConsul)
	must.StrContains(t, warnings, `use provider "consul"`)
	must.StrContains(t, warnings, `no node of region "global" runs a Consul agent`)

	// A node with a Consul agent: no warning.
	withConsul := mock.Node()
	withConsul.Attributes["consul.version"] = "1.21.0"
	must.NoError(t, s1.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 1001, withConsul))
	must.StrNotContains(t, register(structs.ServiceProviderConsul), "Consul agent")
}
