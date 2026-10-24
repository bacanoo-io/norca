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
	"github.com/shoenig/test/wait"
)

func ttlService(id string, ttl time.Duration) *structs.ServiceRegistration {
	return &structs.ServiceRegistration{
		ID: id, ServiceName: "billing-db", Address: "10.0.0.5", Port: 5432, Tags: []string{"primary"},
		Meta: map[string]string{"engine": "postgres"},
		External: &structs.ServiceExternal{
			Kind:   structs.ExternalServiceKindAddress,
			Source: structs.ExternalServiceSourceAPI,
			Checks: []*structs.ExternalServiceCheck{{
				ID: "ttl", Name: "heartbeat", Type: structs.ExternalCheckTypeTTL, TTL: ttl,
				InitialStatus: structs.ServiceHealthPassing,
			}},
		},
	}
}

func upsertExternal(t *testing.T, s *Server, reg *structs.ServiceRegistration, token, nodeSecret string) (
	*structs.ExternalServiceUpsertResponse, error) {
	t.Helper()
	req := &structs.ExternalServiceUpsertRequest{
		Service:      reg,
		NodeSecretID: nodeSecret,
		WriteRequest: structs.WriteRequest{Region: DefaultRegion, Namespace: structs.DefaultNamespace, AuthToken: token},
	}
	var resp structs.ExternalServiceUpsertResponse
	err := msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationUpsertExternalRPCMethod, req, &resp)
	return &resp, err
}

func updateExternalTTL(t *testing.T, s *Server, id, check, status, token string) error {
	t.Helper()
	req := &structs.ExternalServiceCheckUpdateRequest{
		ID:           id,
		Updates:      []*structs.ExternalCheckUpdate{{CheckID: check, Status: status, Output: "from test"}},
		WriteRequest: structs.WriteRequest{Region: DefaultRegion, Namespace: structs.DefaultNamespace, AuthToken: token},
	}
	var resp structs.ExternalServiceCheckUpdateResponse
	return msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationUpdateExternalChecksRPCMethod, req, &resp)
}

func getRegistration(t *testing.T, s *Server, id string) *structs.ServiceRegistration {
	t.Helper()
	reg, err := s.State().GetServiceRegistrationByID(nil, structs.DefaultNamespace, id)
	must.NoError(t, err)
	return reg
}

func TestServiceRegistration_External_Lifecycle(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	// register: the TTL check starts passing
	resp, err := upsertExternal(t, s, ttlService("billing-db-1", time.Minute), "", "")
	must.NoError(t, err)
	must.Eq(t, "billing-db-1", resp.ID)
	reg := getRegistration(t, s, "billing-db-1")
	must.NotNil(t, reg)
	must.Eq(t, "", reg.NodeID)
	must.Eq(t, "", reg.AllocID)
	must.True(t, reg.Ready)
	must.Eq(t, structs.ServiceHealthPassing, reg.Health)
	must.Eq(t, structs.ServiceHealthPassing, reg.CheckState("ttl").Status)

	// it is in the catalog like any registration
	getReq := &structs.ServiceRegistrationByNameRequest{
		ServiceName: "billing-db", HealthyOnly: true,
		QueryOptions: structs.QueryOptions{Region: DefaultRegion, Namespace: structs.DefaultNamespace},
	}
	var getResp structs.ServiceRegistrationByNameResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationGetServiceRPCMethod, getReq, &getResp))
	must.Len(t, 1, getResp.Services)
	must.Eq(t, "10.0.0.5", getResp.Services[0].Address)

	// a renewal with the same status does not write
	index := reg.ModifyIndex
	must.NoError(t, updateExternalTTL(t, s, "billing-db-1", "", structs.ServiceHealthPassing, ""))
	must.Eq(t, index, getRegistration(t, s, "billing-db-1").ModifyIndex)

	// a status change does, with its output
	must.NoError(t, updateExternalTTL(t, s, "billing-db-1", "ttl", structs.ServiceHealthWarning, ""))
	reg = getRegistration(t, s, "billing-db-1")
	must.Eq(t, structs.ServiceHealthWarning, reg.Health)
	must.Eq(t, "from test", reg.CheckState("ttl").Output)

	// bad updates
	must.ErrorContains(t, updateExternalTTL(t, s, "billing-db-1", "nope", structs.ServiceHealthPassing, ""), "not found")
	must.ErrorContains(t, updateExternalTTL(t, s, "billing-db-1", "ttl", "ok", ""), "invalid check status")
	must.ErrorContains(t, updateExternalTTL(t, s, "nope", "", structs.ServiceHealthPassing, ""), "not found")

	// re-registration keeps the state of existing checks
	_, err = upsertExternal(t, s, ttlService("billing-db-1", 2*time.Minute), "", "")
	must.NoError(t, err)
	reg = getRegistration(t, s, "billing-db-1")
	must.Eq(t, structs.ServiceHealthWarning, reg.CheckState("ttl").Status)
	must.Eq(t, 2*time.Minute, reg.External.Checks[0].TTL)

	// list
	listReq := &structs.ExternalServiceListRequest{
		QueryOptions: structs.QueryOptions{Region: DefaultRegion, Namespace: structs.AllNamespacesSentinel},
	}
	var listResp structs.ExternalServiceListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationListExternalRPCMethod, listReq, &listResp))
	must.Len(t, 1, listResp.Services)

	// an allocation's registration ID cannot be taken, and is not external
	allocRegs := mock.ServiceRegistrations()
	allocRegs[0].Namespace = structs.DefaultNamespace
	must.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 5000, allocRegs[:1]))
	_, err = upsertExternal(t, s, ttlService(allocRegs[0].ID, time.Minute), "", "")
	must.ErrorContains(t, err, "used by a registration of allocation")
	delReq := &structs.ExternalServiceDeleteRequest{ID: allocRegs[0].ID,
		WriteRequest: structs.WriteRequest{Region: DefaultRegion, Namespace: structs.DefaultNamespace}}
	var delResp structs.ExternalServiceDeleteResponse
	err = msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationDeleteExternalRPCMethod, delReq, &delResp)
	must.ErrorContains(t, err, "not an external service")

	// http checks need a client node
	httpSvc := ttlService("web-ext", time.Minute)
	httpSvc.External.Checks = []*structs.ExternalServiceCheck{{ID: "http", Type: structs.ExternalCheckTypeHTTP,
		HTTP: "http://10.0.0.5/health"}}
	_, err = upsertExternal(t, s, httpSvc, "", "")
	must.ErrorContains(t, err, "client agent")
	node := mock.Node()
	must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 5001, node))
	_, err = upsertExternal(t, s, httpSvc, "", node.SecretID)
	must.NoError(t, err)
	reg = getRegistration(t, s, "web-ext")
	must.Eq(t, node.ID, reg.NodeID)
	must.Eq(t, node.Datacenter, reg.Datacenter)
	must.Eq(t, structs.DefaultExternalCheckInterval, reg.External.Checks[0].Interval)

	// the node reports the results of its checks; others cannot
	nodeReport := &structs.ExternalServiceCheckUpdateRequest{
		ID:         "web-ext",
		Updates:    []*structs.ExternalCheckUpdate{{CheckID: "http", Status: structs.ServiceHealthCritical, Output: "500"}},
		NodeReport: true,
		WriteRequest: structs.WriteRequest{Region: DefaultRegion, Namespace: structs.DefaultNamespace,
			AuthToken: node.SecretID},
	}
	var updResp structs.ExternalServiceCheckUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationUpdateExternalChecksRPCMethod, nodeReport, &updResp))
	must.Eq(t, structs.ServiceHealthCritical, getRegistration(t, s, "web-ext").Health)
	other := mock.Node()
	must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 5002, other))
	nodeReport.AuthToken = other.SecretID
	err = msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationUpdateExternalChecksRPCMethod, nodeReport, &updResp)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	// http checks are not renewed as TTL checks
	must.ErrorContains(t, updateExternalTTL(t, s, "web-ext", "http", structs.ServiceHealthPassing, ""), "not a TTL check")

	// the node lists its registrations
	listReq = &structs.ExternalServiceListRequest{NodeID: node.ID,
		QueryOptions: structs.QueryOptions{Region: DefaultRegion, Namespace: structs.AllNamespacesSentinel, AuthToken: node.SecretID}}
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationListExternalRPCMethod, listReq, &listResp))
	must.Len(t, 1, listResp.Services)
	must.Eq(t, "web-ext", listResp.Services[0].ID)

	// registrations of a node go away with the node
	must.NoError(t, s.State().DeleteServiceRegistrationByNodeID(structs.MsgTypeTestSetup, 5003, node.ID))
	must.Nil(t, getRegistration(t, s, "web-ext"))

	// deregister
	delReq.ID = "billing-db-1"
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationDeleteExternalRPCMethod, delReq, &delResp))
	must.Nil(t, getRegistration(t, s, "billing-db-1"))
	err = msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationDeleteExternalRPCMethod, delReq, &delResp)
	must.ErrorContains(t, err, "not found")
}

func TestServiceRegistration_External_TTLExpiryAndGC(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	s.externalServices.lock.Lock()
	s.externalServices.reconcileInterval = 200 * time.Millisecond
	s.externalServices.lock.Unlock()

	// expires and turns critical, but is kept
	_, err := upsertExternal(t, s, ttlService("keep", time.Second), "", "")
	must.NoError(t, err)

	// expires, turns critical, and is deregistered a second later
	reap := ttlService("reap", time.Second)
	reap.External.DeregisterCriticalAfter = time.Second
	_, err = upsertExternal(t, s, reap, "", "")
	must.NoError(t, err)

	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			reg := getRegistration(t, s, "keep")
			return reg != nil && reg.Health == structs.ServiceHealthCritical &&
				reg.CheckState("ttl").Output == structs.ExternalCheckTTLExpiredOutput
		}),
		wait.Timeout(10*time.Second), wait.Gap(100*time.Millisecond),
	))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return getRegistration(t, s, "reap") == nil }),
		wait.Timeout(10*time.Second), wait.Gap(100*time.Millisecond),
	))

	// a renewal brings it back
	must.NoError(t, updateExternalTTL(t, s, "keep", "", structs.ServiceHealthPassing, ""))
	must.Eq(t, structs.ServiceHealthPassing, getRegistration(t, s, "keep").Health)

	// kept alive by renewals
	for i := 0; i < 4; i++ {
		time.Sleep(400 * time.Millisecond)
		must.NoError(t, updateExternalTTL(t, s, "keep", "", structs.ServiceHealthPassing, ""))
	}
	must.Eq(t, structs.ServiceHealthPassing, getRegistration(t, s, "keep").Health)

	// a new leader gives every TTL check a full TTL
	s.externalServices.lock.Lock()
	s.externalServices.leader = false
	s.externalServices.lock.Unlock()
	s.externalServices.tick()
	s.externalServices.lock.Lock()
	must.MapLen(t, 1, s.externalServices.deadlines)
	s.externalServices.lock.Unlock()
}

func TestServiceRegistration_External_ACL(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := TestACLServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	register := mock.CreatePolicyAndToken(t, s.State(), 1001, "register",
		mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityRegisterExternalService}))
	write := mock.CreatePolicyAndToken(t, s.State(), 1003, "write",
		mock.NamespacePolicy(structs.DefaultNamespace, acl.PolicyWrite, nil))
	reader := mock.CreatePolicyAndToken(t, s.State(), 1005, "read",
		mock.NamespacePolicy(structs.DefaultNamespace, acl.PolicyRead, nil))
	operator := mock.CreatePolicyAndToken(t, s.State(), 1007, "operator",
		mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityRegisterExternalService})+
			"\noperator { policy = \"write\" }\n")

	// the capability is required, and not part of the write policy
	_, err := upsertExternal(t, s, ttlService("db", time.Minute), "", "")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = upsertExternal(t, s, ttlService("db", time.Minute), write.SecretID, "")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = upsertExternal(t, s, ttlService("db", time.Minute), register.SecretID, "")
	must.NoError(t, err)

	// renewals need it too
	must.ErrorContains(t, updateExternalTTL(t, s, "db", "", structs.ServiceHealthPassing, reader.SecretID),
		structs.ErrPermissionDenied.Error())
	must.NoError(t, updateExternalTTL(t, s, "db", "", structs.ServiceHealthPassing, register.SecretID))

	// a node cannot register with its secret as token
	node := mock.Node()
	must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 1010, node))
	_, err = upsertExternal(t, s, ttlService("db2", time.Minute), node.SecretID, "")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// static registrations (no TTL check, no node) need operator write
	alias := &structs.ServiceRegistration{
		ID: "billing-db", ServiceName: "billing-db", Address: "billing.c9xk2.eu-central-1.rds.amazonaws.com",
		External: &structs.ServiceExternal{Kind: structs.ExternalServiceKindAlias},
	}
	_, err = upsertExternal(t, s, alias.Copy(), register.SecretID, "")
	must.ErrorContains(t, err, "operator write")
	_, err = upsertExternal(t, s, alias.Copy(), operator.SecretID, "")
	must.NoError(t, err)
	_, err = upsertExternal(t, s, alias.Copy(), root.SecretID, "")
	must.NoError(t, err)

	// listing needs read-job
	listReq := &structs.ExternalServiceListRequest{
		QueryOptions: structs.QueryOptions{Region: DefaultRegion, Namespace: structs.DefaultNamespace},
	}
	var listResp structs.ExternalServiceListResponse
	err = msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationListExternalRPCMethod, listReq, &listResp)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	listReq.AuthToken = reader.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationListExternalRPCMethod, listReq, &listResp))
	must.Len(t, 2, listResp.Services)

	// a node lists only its own registrations
	listReq = &structs.ExternalServiceListRequest{NodeID: "other",
		QueryOptions: structs.QueryOptions{Region: DefaultRegion, Namespace: structs.AllNamespacesSentinel, AuthToken: node.SecretID}}
	err = msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationListExternalRPCMethod, listReq, &listResp)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// deregistration needs the capability
	delReq := &structs.ExternalServiceDeleteRequest{ID: "db",
		WriteRequest: structs.WriteRequest{Region: DefaultRegion, Namespace: structs.DefaultNamespace, AuthToken: reader.SecretID}}
	var delResp structs.ExternalServiceDeleteResponse
	err = msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationDeleteExternalRPCMethod, delReq, &delResp)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	delReq.AuthToken = register.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), structs.ServiceRegistrationDeleteExternalRPCMethod, delReq, &delResp))
}
