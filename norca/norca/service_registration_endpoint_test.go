// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"testing"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/hashicorp/go-memdb"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
	"github.com/stretchr/testify/require"
)

func TestServiceRegistration_Upsert(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		serverFn func(t *testing.T) (*Server, *structs.ACLToken, func())
		testFn   func(t *testing.T, s *Server, token *structs.ACLToken)
		name     string
	}{
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations and ensure
				// they are in the same namespace.
				services := mock.ServiceRegistrations()
				services[1].Namespace = services[0].Namespace

				// Attempt to upsert without a token.
				serviceRegReq := &structs.ServiceRegistrationUpsertRequest{
					Services: services,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
					},
				}
				var serviceRegResp structs.ServiceRegistrationUpsertResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationUpsertRPCMethod, serviceRegReq, &serviceRegResp)
				require.Error(t, err)
				require.Contains(t, err.Error(), "node lookup by SecretID failed")

				// Generate a node and retry the upsert.
				node := mock.Node()
				require.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 10, node))

				ws := memdb.NewWatchSet()
				node, err = s.State().NodeByID(ws, node.ID)
				require.NoError(t, err)
				require.NotNil(t, node)
				// a client registers the services of its own allocations
				nodeOwnedRegistrations(t, s, 11, node, services...)

				serviceRegReq.WriteRequest.AuthToken = node.SecretID
				err = msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationUpsertRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.Greater(t, serviceRegResp.Index, uint64(1))
			},
			name: "ACLs disabled without node secret",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations and ensure
				// they are in the same namespace.
				services := mock.ServiceRegistrations()
				services[1].Namespace = services[0].Namespace

				// Generate a node.
				node := mock.Node()
				require.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 10, node))

				ws := memdb.NewWatchSet()
				node, err := s.State().NodeByID(ws, node.ID)
				require.NoError(t, err)
				require.NotNil(t, node)
				// a client registers the services of its own allocations
				nodeOwnedRegistrations(t, s, 11, node, services...)

				serviceRegReq := &structs.ServiceRegistrationUpsertRequest{
					Services: services,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
						AuthToken: node.SecretID,
					},
				}
				var serviceRegResp structs.ServiceRegistrationUpsertResponse
				err = msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationUpsertRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.Greater(t, serviceRegResp.Index, uint64(1))
			},
			name: "ACLs disabled with node secret",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations and ensure
				// they are in the same namespace.
				services := mock.ServiceRegistrations()
				services[1].Namespace = services[0].Namespace

				// Attempt to upsert without a token.
				serviceRegReq := &structs.ServiceRegistrationUpsertRequest{
					Services: services,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
					},
				}
				var serviceRegResp structs.ServiceRegistrationUpsertResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationUpsertRPCMethod, serviceRegReq, &serviceRegResp)
				require.Error(t, err)
				require.Contains(t, err.Error(), "node lookup by SecretID failed")

				// Generate a node and retry the upsert.
				node := mock.Node()
				require.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 10, node))

				ws := memdb.NewWatchSet()
				node, err = s.State().NodeByID(ws, node.ID)
				require.NoError(t, err)
				require.NotNil(t, node)
				// a client registers the services of its own allocations
				nodeOwnedRegistrations(t, s, 11, node, services...)

				serviceRegReq.WriteRequest.AuthToken = node.SecretID
				err = msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationUpsertRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.Greater(t, serviceRegResp.Index, uint64(1))
			},
			name: "ACLs enabled without node secret",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations and ensure
				// they are in the same namespace.
				services := mock.ServiceRegistrations()
				services[1].Namespace = services[0].Namespace

				// Generate a node.
				node := mock.Node()
				require.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 10, node))

				ws := memdb.NewWatchSet()
				node, err := s.State().NodeByID(ws, node.ID)
				require.NoError(t, err)
				require.NotNil(t, node)
				// a client registers the services of its own allocations
				nodeOwnedRegistrations(t, s, 11, node, services...)

				serviceRegReq := &structs.ServiceRegistrationUpsertRequest{
					Services: services,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
						AuthToken: node.SecretID,
					},
				}
				var serviceRegResp structs.ServiceRegistrationUpsertResponse
				err = msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationUpsertRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.Greater(t, serviceRegResp.Index, uint64(1))
			},
			name: "ACLs enabled with node secret",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server, aclToken, cleanup := tc.serverFn(t)
			defer cleanup()
			tc.testFn(t, server, aclToken)
		})
	}
}

// nodeOwnedRegistrations makes regs registrations of allocations of node,
// as the client of the node sends them: an allocation of the node for each,
// in the namespace and job of the registration.
func nodeOwnedRegistrations(t *testing.T, s *Server, index uint64, node *structs.Node,
	regs ...*structs.ServiceRegistration) {
	t.Helper()
	allocs := make([]*structs.Allocation, 0, len(regs))
	for _, reg := range regs {
		alloc := mock.Alloc()
		alloc.NodeID = node.ID
		alloc.Namespace = reg.Namespace
		alloc.Job.Namespace = reg.Namespace
		if reg.JobID == "" {
			reg.JobID = alloc.JobID
		}
		alloc.JobID = reg.JobID
		alloc.Job.ID = reg.JobID
		reg.NodeID = node.ID
		reg.AllocID = alloc.ID
		allocs = append(allocs, alloc)
	}
	must.NoError(t, s.State().UpsertAllocs(structs.MsgTypeTestSetup, index, allocs))
}

// TestServiceRegistration_Upsert_NodeOwnsRegistrations: a client node
// registers the services of its own allocations only, never an external
// service, and replaces neither an external service nor a registration of
// another node (review finding JOG-12).
func TestServiceRegistration_Upsert_NodeOwnsRegistrations(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)
	st := s.State()

	node, other := mock.Node(), mock.Node()
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 10, node))
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 11, other))

	// what is in the state already: a registration of the other node, and
	// an external service
	foreign := mock.ServiceRegistrations()[0]
	foreign.ID = "foreign"
	nodeOwnedRegistrations(t, s, 12, other, foreign)
	external := mock.ServiceRegistrations()[0]
	external.ID, external.NodeID, external.AllocID, external.JobID = "external", "", "", ""
	external.External = &structs.ServiceExternal{Kind: structs.ExternalServiceKindAddress}
	must.NoError(t, st.UpsertServiceRegistrations(structs.MsgTypeTestSetup, 13, []*structs.ServiceRegistration{foreign, external}))

	// valid returns a registration of an allocation of node
	index := uint64(20)
	valid := func(id string) *structs.ServiceRegistration {
		reg := mock.ServiceRegistrations()[0]
		reg.ID = id
		nodeOwnedRegistrations(t, s, index, node, reg)
		index++
		return reg
	}
	upsert := func(reg *structs.ServiceRegistration) error {
		req := &structs.ServiceRegistrationUpsertRequest{
			Services:     []*structs.ServiceRegistration{reg},
			WriteRequest: structs.WriteRequest{Region: DefaultRegion, AuthToken: node.SecretID},
		}
		var resp structs.ServiceRegistrationUpsertResponse
		return msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationUpsertRPCMethod, req, &resp)
	}
	stored := func(reg *structs.ServiceRegistration) *structs.ServiceRegistration {
		out, err := st.GetServiceRegistrationByID(nil, reg.Namespace, reg.ID)
		must.NoError(t, err)
		return out
	}

	refused := []struct {
		name   string
		reg    func() *structs.ServiceRegistration
		reason string
	}{
		{"another node", func() *structs.ServiceRegistration {
			reg := valid("r1")
			reg.NodeID = other.ID
			return reg
		}, structs.ErrPermissionDenied.Error()},
		{"external service", func() *structs.ServiceRegistration {
			reg := valid("r2")
			reg.External = &structs.ServiceExternal{Kind: structs.ExternalServiceKindAddress}
			return reg
		}, "external services API"},
		{"allocation of another node", func() *structs.ServiceRegistration {
			reg := valid("r3")
			reg.AllocID = foreign.AllocID
			return reg
		}, structs.ErrPermissionDenied.Error()},
		{"unknown allocation", func() *structs.ServiceRegistration {
			reg := valid("r4")
			reg.AllocID = uuid.Generate()
			return reg
		}, structs.ErrPermissionDenied.Error()},
		{"namespace of another allocation", func() *structs.ServiceRegistration {
			reg := valid("r5")
			reg.Namespace = "platform"
			return reg
		}, structs.ErrPermissionDenied.Error()},
		{"job of another allocation", func() *structs.ServiceRegistration {
			reg := valid("r6")
			reg.JobID = "other-job"
			return reg
		}, structs.ErrPermissionDenied.Error()},
		{"replaces a registration of another node", func() *structs.ServiceRegistration {
			reg := valid("foreign")
			return reg
		}, structs.ErrPermissionDenied.Error()},
		{"replaces an external service", func() *structs.ServiceRegistration {
			reg := valid("external")
			return reg
		}, structs.ErrPermissionDenied.Error()},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			reg := tc.reg()
			before := stored(reg)
			must.ErrorContains(t, upsert(reg), tc.reason)
			must.Eq(t, before, stored(reg))
		})
	}

	t.Run("own allocation", func(t *testing.T) {
		reg := valid("own")
		must.NoError(t, upsert(reg))
		must.NotNil(t, stored(reg))
	})
}

func TestServiceRegistration_DeleteByID(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		serverFn func(t *testing.T) (*Server, *structs.ACLToken, func())
		testFn   func(t *testing.T, s *Server, token *structs.ACLToken)
		name     string
	}{
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Attempt to delete a service registration that does not
				// exist.
				serviceRegReq := &structs.ServiceRegistrationDeleteByIDRequest{
					ID: "this-is-not-the-service-you're-looking-for",
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: "default",
					},
				}

				var serviceRegResp structs.ServiceRegistrationDeleteByIDResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationDeleteByIDRPCMethod, serviceRegReq, &serviceRegResp)
				require.Error(t, err)
				require.Contains(t, err.Error(), "service registration not found")
			},
			name: "ACLs disabled unknown service",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Try and delete one of the services that exist.
				serviceRegReq := &structs.ServiceRegistrationDeleteByIDRequest{
					ID: services[0].ID,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
					},
				}

				var serviceRegResp structs.ServiceRegistrationDeleteByIDResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationDeleteByIDRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
			},
			name: "ACLs disabled known service",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Try and delete one of the services that exist.
				serviceRegReq := &structs.ServiceRegistrationDeleteByIDRequest{
					ID: services[0].ID,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
						AuthToken: token.SecretID,
					},
				}

				var serviceRegResp structs.ServiceRegistrationDeleteByIDResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationDeleteByIDRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
			},
			name: "ACLs enabled known service with management token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Try and delete one of the services that exist but don't set
				// an auth token.
				serviceRegReq := &structs.ServiceRegistrationDeleteByIDRequest{
					ID: services[0].ID,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
					},
				}

				var serviceRegResp structs.ServiceRegistrationDeleteByIDResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationDeleteByIDRPCMethod, serviceRegReq, &serviceRegResp)
				require.Error(t, err)
				require.Contains(t, err.Error(), "Permission denied")
			},
			name: "ACLs enabled known service without token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Create a token using submit-job capability.
				authToken := mock.CreatePolicyAndToken(t, s.State(), 30, "test-service-reg-delete",
					mock.NamespacePolicy(services[0].Namespace, "", []string{acl.NamespaceCapabilitySubmitJob})).SecretID

				// Try and delete one of the services that exist but don't set
				// an auth token.
				serviceRegReq := &structs.ServiceRegistrationDeleteByIDRequest{
					ID: services[0].ID,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
						AuthToken: authToken,
					},
				}

				var serviceRegResp structs.ServiceRegistrationDeleteByIDResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationDeleteByIDRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
			},
			name: "ACLs enabled known service with submit-job namespace token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Create a token using submit-job capability.
				authToken := mock.CreatePolicyAndToken(t, s.State(), 30, "test-service-reg-delete",
					mock.NamespacePolicy(services[0].Namespace, "", []string{acl.NamespaceCapabilityReadJob})).SecretID

				// Try and delete one of the services that exist but don't set
				// an auth token.
				serviceRegReq := &structs.ServiceRegistrationDeleteByIDRequest{
					ID: services[0].ID,
					WriteRequest: structs.WriteRequest{
						Region:    DefaultRegion,
						Namespace: services[0].Namespace,
						AuthToken: authToken,
					},
				}

				var serviceRegResp structs.ServiceRegistrationDeleteByIDResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationDeleteByIDRPCMethod, serviceRegReq, &serviceRegResp)
				require.Error(t, err)
				require.Contains(t, err.Error(), "Permission denied")
			},
			name: "ACLs enabled known service with read-job namespace token",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server, aclToken, cleanup := tc.serverFn(t)
			defer cleanup()
			tc.testFn(t, server, aclToken)
		})
	}
}

func TestServiceRegistration_List(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		serverFn func(t *testing.T) (*Server, *structs.ACLToken, func())
		testFn   func(t *testing.T, s *Server, token *structs.ACLToken)
		name     string
	}{
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: structs.AllNamespacesSentinel,
						Region:    DefaultRegion,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "default",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "example-cache",
								Tags:        []string{"foo"},
							},
						}},
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs disabled wildcard ns",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "platform",
						Region:    DefaultRegion,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						},
					},
				}, serviceRegResp.Services)
			},
			name: "ACLs disabled platform ns",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "platform",
						Region:    DefaultRegion,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{}, serviceRegResp.Services)
			},
			name: "ACLs disabled no services",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: structs.AllNamespacesSentinel,
						Region:    DefaultRegion,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{}, serviceRegResp.Services)
			},
			name: "ACLs enabled wildcard ns without token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "default",
						Region:    DefaultRegion,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.Error(t, err)
				require.Contains(t, err.Error(), "Permission denied")
			},
			name: "ACLs enabled default ns without token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: structs.AllNamespacesSentinel,
						Region:    DefaultRegion,
						AuthToken: token.SecretID,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "default",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "example-cache",
								Tags:        []string{"foo"},
							},
						}},
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs enabled wildcard with management token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "default",
						Region:    DefaultRegion,
						AuthToken: token.SecretID,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "default",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "example-cache",
								Tags:        []string{"foo"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs enabled default ns with management token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Create a policy and grab the token which has the read-job
				// capability on the platform namespace.
				customToken := mock.CreatePolicyAndToken(t, s.State(), 5, "test-valid-autoscaler",
					mock.NamespacePolicy("platform", "", []string{acl.NamespaceCapabilityReadJob})).SecretID

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "platform",
						Region:    DefaultRegion,
						AuthToken: customToken,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs enabled with read-job policy token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Create a namespace as this is needed when using an ACL like
				// we do in this test.
				ns := &structs.Namespace{
					Name:        "platform",
					Description: "test namespace",
					CreateIndex: 5,
					ModifyIndex: 5,
				}
				ns.SetHash()
				require.NoError(t, s.State().UpsertNamespaces(5, []*structs.Namespace{ns}))

				// Create a policy and grab the token which has the read-job
				// capability on the platform namespace.
				customToken := mock.CreatePolicyAndToken(t, s.State(), 10, "test-valid-autoscaler",
					mock.NamespacePolicy("platform", "", []string{acl.NamespaceCapabilityReadJob})).SecretID

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 20, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: structs.AllNamespacesSentinel,
						Region:    DefaultRegion,
						AuthToken: customToken,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs enabled wildcard ns with restricted token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Create a namespace as this is needed when using an ACL like
				// we do in this test.
				ns := &structs.Namespace{
					Name:        "platform",
					Description: "test namespace",
					CreateIndex: 5,
					ModifyIndex: 5,
				}
				ns.SetHash()
				require.NoError(t, s.State().UpsertNamespaces(5, []*structs.Namespace{ns}))

				// Create a policy and grab the token which has the read policy
				// on the platform namespace.
				customToken := mock.CreatePolicyAndToken(t, s.State(), 10, "test-valid-autoscaler",
					mock.NamespacePolicy("platform", "read", nil)).SecretID

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 20, services))

				// Test a request without setting an ACL token.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: structs.AllNamespacesSentinel,
						Region:    DefaultRegion,
						AuthToken: customToken,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs enabled with read namespace policy token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Create a namespace as this is needed when using an ACL like
				// we do in this test.
				ns := &structs.Namespace{
					Name:        "platform",
					Description: "test namespace",
					CreateIndex: 5,
					ModifyIndex: 5,
				}
				ns.SetHash()
				require.NoError(t, s.State().UpsertNamespaces(5, []*structs.Namespace{ns}))

				// Generate a node.
				node := mock.Node()
				require.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 10, node))

				ws := memdb.NewWatchSet()
				node, err := s.State().NodeByID(ws, node.ID)
				require.NoError(t, err)
				require.NotNil(t, node)

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 20, services))

				// Test a request while setting the auth token to the node
				// secret ID.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "platform",
						Region:    DefaultRegion,
						AuthToken: node.SecretID,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err = msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)

				// The node secret ID reads every namespace with the wildcard.
				serviceRegReq.QueryOptions.Namespace = structs.AllNamespacesSentinel
				var wildcardResp structs.ServiceRegistrationListResponse
				err = msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &wildcardResp)
				require.NoError(t, err)
				namespaces := make([]string, 0, len(wildcardResp.Services))
				for _, stub := range wildcardResp.Services {
					namespaces = append(namespaces, stub.Namespace)
				}
				require.ElementsMatch(t, []string{structs.DefaultNamespace, "platform"}, namespaces)
			},
			name: "ACLs enabled with node secret token",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Create a namespace as this is needed when using an ACL like
				// we do in this test.
				ns := &structs.Namespace{
					Name:        "platform",
					Description: "test namespace",
					CreateIndex: 5,
					ModifyIndex: 5,
				}
				ns.SetHash()
				require.NoError(t, s.State().UpsertNamespaces(5, []*structs.Namespace{ns}))

				// Generate an allocation with a signed identity
				allocs := []*structs.Allocation{mock.Alloc()}
				job := allocs[0].Job
				job.Namespace = "platform"
				allocs[0].Namespace = "platform"
				require.NoError(t, s.State().UpsertJob(structs.MsgTypeTestSetup, 10, nil, job))
				signAllocIdentities(s.encrypter, job, allocs)
				require.NoError(t, s.State().UpsertAllocs(structs.MsgTypeTestSetup, 15, allocs))

				signedToken := allocs[0].SignedIdentities["web"]

				// Associate an unrelated policy with the identity's job to
				// ensure it doesn't conflict.
				policy := &structs.ACLPolicy{
					Name:  "policy-for-identity",
					Rules: mock.NodePolicy("read"),
					JobACL: &structs.JobACL{
						Namespace: "platform",
						JobID:     job.ID,
					},
				}
				policy.SetHash()
				must.NoError(t, s.State().UpsertACLPolicies(structs.MsgTypeTestSetup, 16,
					[]*structs.ACLPolicy{policy}))

				// Generate and upsert some service registrations.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 20, services))

				// Test a request while setting the auth token to the signed token
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "platform",
						Region:    DefaultRegion,
						AuthToken: signedToken,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod,
					serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs enabled with valid signed identity",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server, aclToken, cleanup := tc.serverFn(t)
			defer cleanup()
			tc.testFn(t, server, aclToken)
		})
	}
}

func TestServiceRegistration_GetService(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		serverFn func(t *testing.T) (*Server, *structs.ACLToken, func())
		testFn   func(t *testing.T, s *Server, token *structs.ACLToken)
		name     string
	}{
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, _ *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate mock services then upsert them individually using different indexes.
				services := mock.ServiceRegistrations()

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 10, []*structs.ServiceRegistration{services[0]}))

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 20, []*structs.ServiceRegistration{services[1]}))

				// Lookup the first registration.
				serviceRegReq := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[0].Namespace,
						Region:    s.Region(),
					},
				}
				var serviceRegResp structs.ServiceRegistrationByNameResponse
				err := msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.Equal(t, uint64(10), serviceRegResp.Services[0].CreateIndex)
				require.Equal(t, uint64(20), serviceRegResp.Index)
				require.Len(t, serviceRegResp.Services, 1)

				// Lookup the second registration.
				serviceRegReq2 := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[1].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[1].Namespace,
						Region:    s.Region(),
					},
				}
				var serviceRegResp2 structs.ServiceRegistrationByNameResponse
				err = msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq2, &serviceRegResp2)
				require.NoError(t, err)
				require.Equal(t, uint64(20), serviceRegResp2.Services[0].CreateIndex)
				require.Equal(t, uint64(20), serviceRegResp.Index)
				require.Len(t, serviceRegResp2.Services, 1)

				// Perform a lookup with namespace and service name that shouldn't produce
				// results.
				serviceRegReq3 := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[1].Namespace,
						Region:    s.Region(),
					},
				}
				var serviceRegResp3 structs.ServiceRegistrationByNameResponse
				err = msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq3, &serviceRegResp3)
				require.NoError(t, err)
				require.Len(t, serviceRegResp3.Services, 0)
			},
			name: "ACLs disabled",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate mock services then upsert them individually using different indexes.
				services := mock.ServiceRegistrations()

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 10, []*structs.ServiceRegistration{services[0]}))

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 20, []*structs.ServiceRegistration{services[1]}))

				// Lookup the first registration without using an ACL token
				// which should fail.
				serviceRegReq := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[0].Namespace,
						Region:    s.Region(),
					},
				}
				var serviceRegResp structs.ServiceRegistrationByNameResponse
				err := msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq, &serviceRegResp)
				require.Error(t, err)
				require.Contains(t, err.Error(), "Permission denied")

				// Lookup the first registration using the management token.
				serviceRegReq2 := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[0].Namespace,
						Region:    s.Region(),
						AuthToken: token.SecretID,
					},
				}
				var serviceRegResp2 structs.ServiceRegistrationByNameResponse
				err = msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq2, &serviceRegResp2)
				require.Nil(t, err)
				require.Len(t, serviceRegResp2.Services, 1)
				require.EqualValues(t, 20, serviceRegResp2.Index)

				// Create a read policy for the default namespace and test this
				// can correctly read the first service.
				authToken1 := mock.CreatePolicyAndToken(t, s.State(), 30, "test-service-reg-get",
					mock.NamespacePolicy(structs.DefaultNamespace, "read", nil)).SecretID
				serviceRegReq3 := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[0].Namespace,
						Region:    s.Region(),
						AuthToken: authToken1,
					},
				}
				var serviceRegResp3 structs.ServiceRegistrationByNameResponse
				err = msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq3, &serviceRegResp3)
				require.Nil(t, err)
				require.Len(t, serviceRegResp3.Services, 1)
				require.EqualValues(t, 20, serviceRegResp2.Index)

				// Attempting to lookup services in a different namespace should fail.
				serviceRegReq4 := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[1].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[1].Namespace,
						Region:    s.Region(),
						AuthToken: authToken1,
					},
				}
				var serviceRegResp4 structs.ServiceRegistrationByNameResponse
				err = msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq4, &serviceRegResp4)
				require.Error(t, err)
				require.Contains(t, err.Error(), "Permission denied")
			},
			name: "ACLs enabled",
		},

		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate mock services then upsert them individually using different indexes.
				services := mock.ServiceRegistrations()

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 10, []*structs.ServiceRegistration{services[0]}))

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 20, []*structs.ServiceRegistration{services[1]}))

				// Generate a node.
				node := mock.Node()
				require.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 30, node))

				ws := memdb.NewWatchSet()
				node, err := s.State().NodeByID(ws, node.ID)
				require.NoError(t, err)
				require.NotNil(t, node)

				// Test a request while setting the auth token to the node
				// secret ID.
				serviceRegReq := &structs.ServiceRegistrationListRequest{
					QueryOptions: structs.QueryOptions{
						Namespace: "platform",
						Region:    DefaultRegion,
						AuthToken: node.SecretID,
					},
				}
				var serviceRegResp structs.ServiceRegistrationListResponse
				err = msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationListRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistrationListStub{
					{
						Namespace: "platform",
						Services: []*structs.ServiceRegistrationStub{
							{
								ServiceName: "countdash-api",
								Tags:        []string{"bar"},
							},
						}},
				}, serviceRegResp.Services)
			},
			name: "ACLs enabled using node secret",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				return TestACLServer(t, nil)
			},
			testFn: func(t *testing.T, s *Server, token *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate mock services then upsert them individually using different indexes.
				services := mock.ServiceRegistrations()

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 10, []*structs.ServiceRegistration{services[0]}))

				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 20, []*structs.ServiceRegistration{services[1]}))

				// Generate an allocation with a signed identity
				allocs := []*structs.Allocation{mock.Alloc()}
				job := allocs[0].Job
				require.NoError(t, s.State().UpsertJob(structs.MsgTypeTestSetup, 10, nil, job))
				signAllocIdentities(s.encrypter, job, allocs)
				require.NoError(t, s.State().UpsertAllocs(structs.MsgTypeTestSetup, 15, allocs))

				signedToken := allocs[0].SignedIdentities["web"]

				// Lookup the first registration.
				serviceRegReq := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[0].Namespace,
						Region:    s.Region(),
						AuthToken: signedToken,
					},
				}
				var serviceRegResp structs.ServiceRegistrationByNameResponse
				err := msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.Equal(t, uint64(10), serviceRegResp.Services[0].CreateIndex)
				require.Equal(t, uint64(20), serviceRegResp.Index)
				require.Len(t, serviceRegResp.Services, 1)
			},
			name: "ACLs enabled using valid signed identity",
		},
		{
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, _ *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// Generate mock services then upsert them individually using different indexes.
				services := mock.ServiceRegistrations()
				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 10, services))

				// Generate a second set of mocks. Set the datacenter to the
				// opposite or the mock, (dc1,dc2) which will be used to test
				// filtering and alter the ID.
				nextServices := mock.ServiceRegistrations()
				nextServices[0].ID += "_next"
				nextServices[0].Datacenter = "dc2"
				nextServices[1].ID += "_next"
				nextServices[1].Datacenter = "dc1"
				require.NoError(t, s.fsm.State().UpsertServiceRegistrations(
					structs.MsgTypeTestSetup, 20, nextServices))

				// Create and test a request where we filter for service
				// registrations in the default namespace, running within
				// datacenter "dc2" only.
				serviceRegReq := &structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: structs.DefaultNamespace,
						Region:    DefaultRegion,
						Filter:    `Datacenter == "dc2"`,
					},
				}
				var serviceRegResp structs.ServiceRegistrationByNameResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq, &serviceRegResp)
				require.NoError(t, err)
				require.ElementsMatch(t, []*structs.ServiceRegistration{nextServices[0]}, serviceRegResp.Services)

				// Create a test function which can be used for each namespace
				// to ensure cross-namespace functionality of pagination.
				namespaceTestFn := func(
					req *structs.ServiceRegistrationByNameRequest,
					resp *structs.ServiceRegistrationByNameResponse) {

					// We have two service registrations, therefore loop twice in
					// order to check the return array and pagination details.
					for i := 0; i < 2; i++ {

						// The message makes debugging test failures easier as we
						// are inside a loop.
						msg := fmt.Sprintf("iteration %v of 2", i)

						err2 := msgpackrpc.CallWithCodec(
							codec, structs.ServiceRegistrationGetServiceRPCMethod, req, resp)
						require.NoError(t, err2, msg)
						require.Len(t, resp.Services, 1, msg)

						// Anything but the first iteration should result in an
						// empty token as we only have two entries.
						switch i {
						case 1:
							require.Empty(t, resp.NextToken)
						default:
							require.NotEmpty(t, resp.NextToken)
							req.NextToken = resp.NextToken
						}
					}
				}

				// Test the default namespace pagnination.
				serviceRegReq2 := structs.ServiceRegistrationByNameRequest{
					ServiceName: services[0].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: structs.DefaultNamespace,
						Region:    DefaultRegion,
						PerPage:   1,
					},
				}
				var serviceRegResp2 structs.ServiceRegistrationByNameResponse
				namespaceTestFn(&serviceRegReq2, &serviceRegResp2)

				// Test the platform namespace pagnination.
				serviceRegReq3 := structs.ServiceRegistrationByNameRequest{
					ServiceName: services[1].ServiceName,
					QueryOptions: structs.QueryOptions{
						Namespace: services[1].Namespace,
						Region:    DefaultRegion,
						PerPage:   1,
					},
				}
				var serviceRegResp3 structs.ServiceRegistrationByNameResponse
				namespaceTestFn(&serviceRegReq3, &serviceRegResp3)

			},
			name: "filtering and pagination",
		},
		{
			name: "choose 2 of 3",
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, _ *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// insert 3 instances of service s1
				nodeID, jobID, allocID := "node_id", "job_id", "alloc_id"
				services := []*structs.ServiceRegistration{
					{
						ID:          "id_1",
						Namespace:   "default",
						ServiceName: "s1",
						NodeID:      nodeID,
						Datacenter:  "dc1",
						JobID:       jobID,
						AllocID:     allocID,
						Tags:        []string{"tag1"},
						Address:     "10.0.0.1",
						Port:        9001,
						CreateIndex: 101,
						ModifyIndex: 201,
					},
					{
						ID:          "id_2",
						Namespace:   "default",
						ServiceName: "s1",
						NodeID:      nodeID,
						Datacenter:  "dc1",
						JobID:       jobID,
						AllocID:     allocID,
						Tags:        []string{"tag2"},
						Address:     "10.0.0.2",
						Port:        9002,
						CreateIndex: 102,
						ModifyIndex: 202,
					},
					{
						ID:          "id_3",
						Namespace:   "default",
						ServiceName: "s1",
						NodeID:      nodeID,
						Datacenter:  "dc1",
						JobID:       jobID,
						AllocID:     allocID,
						Tags:        []string{"tag3"},
						Address:     "10.0.0.3",
						Port:        9003,
						CreateIndex: 103,
						ModifyIndex: 103,
					},
				}
				must.NoError(t, s.fsm.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				serviceRegReq := &structs.ServiceRegistrationByNameRequest{
					ServiceName: "s1",
					Choose:      "2|abc123", // select 2 in consistent order
					QueryOptions: structs.QueryOptions{
						Namespace: structs.DefaultNamespace,
						Region:    DefaultRegion,
					},
				}
				var serviceRegResp structs.ServiceRegistrationByNameResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq, &serviceRegResp)
				must.NoError(t, err)

				result := serviceRegResp.Services

				must.Len(t, 2, result)
				must.Eq(t, "10.0.0.3", result[0].Address)
				must.Eq(t, "10.0.0.2", result[1].Address)
			},
		},
		{
			name: "choose 3 of 2", // gracefully handle requesting too many
			serverFn: func(t *testing.T) (*Server, *structs.ACLToken, func()) {
				server, cleanup := TestServer(t, nil)
				return server, nil, cleanup
			},
			testFn: func(t *testing.T, s *Server, _ *structs.ACLToken) {
				codec := rpcClient(t, s)
				testutil.WaitForLeader(t, s.RPC)

				// insert 2 instances of service s1
				nodeID, jobID, allocID := "node_id", "job_id", "alloc_id"
				services := []*structs.ServiceRegistration{
					{
						ID:          "id_1",
						Namespace:   "default",
						ServiceName: "s1",
						NodeID:      nodeID,
						Datacenter:  "dc1",
						JobID:       jobID,
						AllocID:     allocID,
						Tags:        []string{"tag1"},
						Address:     "10.0.0.1",
						Port:        9001,
						CreateIndex: 101,
						ModifyIndex: 201,
					},
					{
						ID:          "id_2",
						Namespace:   "default",
						ServiceName: "s1",
						NodeID:      nodeID,
						Datacenter:  "dc1",
						JobID:       jobID,
						AllocID:     allocID,
						Tags:        []string{"tag2"},
						Address:     "10.0.0.2",
						Port:        9002,
						CreateIndex: 102,
						ModifyIndex: 202,
					},
				}
				must.NoError(t, s.fsm.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

				serviceRegReq := &structs.ServiceRegistrationByNameRequest{
					ServiceName: "s1",
					Choose:      "3|abc123", // select 3 in consistent order (though there are only 2 total)
					QueryOptions: structs.QueryOptions{
						Namespace: structs.DefaultNamespace,
						Region:    DefaultRegion,
					},
				}
				var serviceRegResp structs.ServiceRegistrationByNameResponse
				err := msgpackrpc.CallWithCodec(
					codec, structs.ServiceRegistrationGetServiceRPCMethod, serviceRegReq, &serviceRegResp)
				must.NoError(t, err)

				result := serviceRegResp.Services

				must.Len(t, 2, result)
				must.Eq(t, "10.0.0.2", result[0].Address)
				must.Eq(t, "10.0.0.1", result[1].Address)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server, aclToken, cleanup := tc.serverFn(t)
			defer cleanup()
			tc.testFn(t, server, aclToken)
		})
	}
}

// TestServiceRegistration_WorkloadIdentityNamespace: a workload identity
// reads the services of its own namespace (template lookups) and of the
// namespaces its policies give read-job in, not those of every namespace
// (review finding JOG-10).
func TestServiceRegistration_WorkloadIdentityNamespace(t *testing.T) {
	ci.Parallel(t)

	s, _, cleanup := TestACLServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)
	waitForActiveRootKey(t, s)
	st := s.State()

	ns := &structs.Namespace{Name: "platform"}
	ns.SetHash()
	must.NoError(t, st.UpsertNamespaces(5, []*structs.Namespace{ns}))

	// services[0] is in the default namespace, services[1] in platform
	services := mock.ServiceRegistrations()
	must.NoError(t, st.UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, services))

	// a workload of the default namespace
	identity := func(t *testing.T, index uint64) (*structs.Job, string) {
		allocs := []*structs.Allocation{mock.Alloc()}
		job := allocs[0].Job
		must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))
		must.NoError(t, signAllocIdentities(s.encrypter, job, allocs))
		must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, index+1, allocs))
		return job, allocs[0].SignedIdentities["web"]
	}

	getService := func(token string, reg *structs.ServiceRegistration) error {
		req := &structs.ServiceRegistrationByNameRequest{ServiceName: reg.ServiceName,
			QueryOptions: structs.QueryOptions{Namespace: reg.Namespace, Region: s.Region(), AuthToken: token}}
		var resp structs.ServiceRegistrationByNameResponse
		return msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationGetServiceRPCMethod, req, &resp)
	}
	list := func(token, namespace string) error {
		req := &structs.ServiceRegistrationListRequest{
			QueryOptions: structs.QueryOptions{Namespace: namespace, Region: s.Region(), AuthToken: token}}
		var resp structs.ServiceRegistrationListResponse
		return msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationListRPCMethod, req, &resp)
	}

	t.Run("own namespace", func(t *testing.T) {
		_, token := identity(t, 20)
		must.NoError(t, getService(token, services[0]))
		must.NoError(t, list(token, services[0].Namespace))
	})
	t.Run("other namespace", func(t *testing.T) {
		_, token := identity(t, 30)
		must.ErrorContains(t, getService(token, services[1]), structs.ErrPermissionDenied.Error())
		must.ErrorContains(t, list(token, services[1].Namespace), structs.ErrPermissionDenied.Error())
	})
	t.Run("other namespace allowed by a policy", func(t *testing.T) {
		job, token := identity(t, 40)
		policy := &structs.ACLPolicy{
			Name:   "platform-read",
			Rules:  mock.NamespacePolicy("platform", acl.PolicyRead, nil),
			JobACL: &structs.JobACL{Namespace: job.Namespace, JobID: job.ID},
		}
		policy.SetHash()
		must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 50, []*structs.ACLPolicy{policy}))
		must.NoError(t, getService(token, services[1]))
		must.NoError(t, list(token, services[1].Namespace))
	})
}

func TestServiceRegistration_chooseErr(t *testing.T) {
	ci.Parallel(t)

	sr := (*ServiceRegistration)(nil)
	try := func(input []*structs.ServiceRegistration, parameter string) {
		result, err := sr.choose(input, parameter)
		must.SliceEmpty(t, result)
		must.ErrorIs(t, err, structs.ErrMalformedChooseParameter)
	}

	regs := []*structs.ServiceRegistration{
		{ID: "abc001", ServiceName: "s1"},
		{ID: "abc002", ServiceName: "s2"},
		{ID: "abc003", ServiceName: "s3"},
	}

	try(regs, "")
	try(regs, "1|")
	try(regs, "|abc")
	try(regs, "a|abc")
}

func TestServiceRegistration_choose(t *testing.T) {
	ci.Parallel(t)

	sr := (*ServiceRegistration)(nil)
	try := func(input, exp []*structs.ServiceRegistration, parameter string) {
		result, err := sr.choose(input, parameter)
		must.NoError(t, err)
		must.Eq(t, exp, result)
	}

	// zero services
	try(nil, []*structs.ServiceRegistration{}, "1|aaa")
	try(nil, []*structs.ServiceRegistration{}, "2|aaa")

	// some unique services
	regs := []*structs.ServiceRegistration{
		{ID: "abc001", ServiceName: "s1"},
		{ID: "abc002", ServiceName: "s1"},
		{ID: "abc003", ServiceName: "s1"},
	}

	// same key, increasing n -> maintains order (n=1)
	try(regs, []*structs.ServiceRegistration{
		{ID: "abc002", ServiceName: "s1"},
	}, "1|aaa")

	// same key, increasing n -> maintains order (n=2)
	try(regs, []*structs.ServiceRegistration{
		{ID: "abc002", ServiceName: "s1"},
		{ID: "abc003", ServiceName: "s1"},
	}, "2|aaa")

	// same key, increasing n -> maintains order (n=3)
	try(regs, []*structs.ServiceRegistration{
		{ID: "abc002", ServiceName: "s1"},
		{ID: "abc003", ServiceName: "s1"},
		{ID: "abc001", ServiceName: "s1"},
	}, "3|aaa")

	// unique key -> different orders
	try(regs, []*structs.ServiceRegistration{
		{ID: "abc001", ServiceName: "s1"},
		{ID: "abc002", ServiceName: "s1"},
		{ID: "abc003", ServiceName: "s1"},
	}, "3|bbb")

	// another key -> another order
	try(regs, []*structs.ServiceRegistration{
		{ID: "abc002", ServiceName: "s1"},
		{ID: "abc003", ServiceName: "s1"},
		{ID: "abc001", ServiceName: "s1"},
	}, "3|ccc")
}
