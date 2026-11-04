// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

func checkedNode(t *testing.T, s *Server, index uint64, pool, dc string) *structs.Node {
	t.Helper()
	node := mock.Node()
	node.NodePool = pool
	node.Datacenter = dc
	must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, index, node))
	return node
}

func declaredHTTPService(id string, cb *structs.ExternalCheckedBy) *structs.ServiceRegistration {
	return &structs.ServiceRegistration{
		ID: id, ServiceName: id, Address: "10.0.0.7", Port: 443,
		External: &structs.ServiceExternal{
			Kind:      structs.ExternalServiceKindAddress,
			Source:    structs.ExternalServiceSourceApply,
			CheckedBy: cb,
			Checks: []*structs.ExternalServiceCheck{{
				ID: "http", Type: structs.ExternalCheckTypeHTTP, HTTP: "https://10.0.0.7/health",
				Interval: 10 * time.Second, Timeout: 2 * time.Second,
				InitialStatus: structs.ServiceHealthPassing,
			}},
		},
	}
}

func TestPickCheckerNode(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	edge1 := checkedNode(t, s, 1000, "default", "dc1")
	edge2 := checkedNode(t, s, 1001, "default", "dc2")
	other := checkedNode(t, s, 1002, "default", "eu-west")

	pick := func(id string, cb *structs.ExternalCheckedBy) *structs.Node {
		node, err := pickCheckerNode(s.State(), id, cb)
		must.NoError(t, err)
		return node
	}

	// a registration always gets the same node
	first := pick("orders", &structs.ExternalCheckedBy{})
	must.NotNil(t, first)
	for i := 0; i < 5; i++ {
		must.Eq(t, first.ID, pick("orders", &structs.ExternalCheckedBy{}).ID)
	}

	// datacenters are glob patterns
	must.Eq(t, other.ID, pick("orders", &structs.ExternalCheckedBy{Datacenters: []string{"eu-*"}}).ID)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		got := pick(id, &structs.ExternalCheckedBy{Datacenters: []string{"dc1", "dc2"}})
		must.True(t, got.ID == edge1.ID || got.ID == edge2.ID)
	}

	// no node of the pool or datacenters: none
	must.Nil(t, pick("orders", &structs.ExternalCheckedBy{NodePool: "nope"}))
	must.Nil(t, pick("orders", &structs.ExternalCheckedBy{Datacenters: []string{"asia"}}))

	// a node that is down, ineligible or draining is skipped
	must.NoError(t, s.State().UpdateNodeStatus(structs.MsgTypeTestSetup, 1010, other.ID, structs.NodeStatusDown, 0, nil))
	must.Nil(t, pick("orders", &structs.ExternalCheckedBy{Datacenters: []string{"eu-*"}}))

	must.NoError(t, s.State().UpdateNodeStatus(structs.MsgTypeTestSetup, 1011, other.ID, structs.NodeStatusReady, 0, nil))
	must.NoError(t, s.State().UpdateNodeEligibility(structs.MsgTypeTestSetup, 1012, other.ID, structs.NodeSchedulingIneligible, 0, nil))
	must.Nil(t, pick("orders", &structs.ExternalCheckedBy{Datacenters: []string{"eu-*"}}))

	// losing a node moves only the registrations it ran
	moved := 0
	var winners = make(map[string]string)
	all := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	for _, id := range all {
		winners[id] = pick(id, &structs.ExternalCheckedBy{}).ID
	}
	must.NoError(t, s.State().UpdateNodeStatus(structs.MsgTypeTestSetup, 1013, edge1.ID, structs.NodeStatusDown, 0, nil))
	for _, id := range all {
		now := pick(id, &structs.ExternalCheckedBy{}).ID
		if winners[id] != edge1.ID {
			must.Eq(t, winners[id], now, must.Sprintf("registration %s moved without a reason", id))
		} else {
			moved++
			must.NotEq(t, edge1.ID, now)
		}
	}
	must.Positive(t, moved)
}

func TestServiceRegistration_External_CheckedBy(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	edge1 := checkedNode(t, s, 1000, "default", "dc1")
	edge2 := checkedNode(t, s, 1001, "default", "dc1")

	var now = time.Now()
	e := s.externalServices
	e.lock.Lock()
	e.checkerInterval = 0
	e.now = func() time.Time { return now }
	e.lock.Unlock()
	tick := func() {
		t.Helper()
		e.tick()
	}

	// the servers choose the node, which runs the checks
	_, err := upsertExternal(t, s, declaredHTTPService("portal", &structs.ExternalCheckedBy{NodePool: "default"}), "", "")
	must.NoError(t, err)
	reg := getRegistration(t, s, "portal")
	must.NotNil(t, reg)
	want, err := pickCheckerNode(s.State(), "portal", reg.External.CheckedBy)
	must.NoError(t, err)
	must.Eq(t, want.ID, reg.NodeID)
	must.Eq(t, "dc1", reg.Datacenter)
	must.Eq(t, structs.ServiceHealthPassing, reg.Health)

	// a node cannot be given together with checked_by
	_, err = upsertExternal(t, s, declaredHTTPService("portal", &structs.ExternalCheckedBy{}), "", edge1.SecretID)
	must.ErrorContains(t, err, "checked_by cannot be combined")

	// the node reports; it keeps the service healthy
	owner := reg.NodeID
	_, err = e.updateChecks(structs.DefaultNamespace, "portal",
		[]*structs.ExternalCheckUpdate{{CheckID: "http", Status: structs.ServiceHealthPassing}}, owner)
	must.NoError(t, err)

	// the node goes down: the service stays and loses the node
	must.NoError(t, s.State().UpdateNodeStatus(structs.MsgTypeTestSetup, 1020, owner, structs.NodeStatusDown, 0, nil))
	must.NoError(t, s.State().DeleteServiceRegistrationByNodeID(structs.MsgTypeTestSetup, 1021, owner))
	reg = getRegistration(t, s, "portal")
	must.NotNil(t, reg)
	must.Eq(t, "", reg.NodeID)

	// the leader gives it to the other node
	tick()
	reg = getRegistration(t, s, "portal")
	other := edge1.ID
	if owner == edge1.ID {
		other = edge2.ID
	}
	must.Eq(t, other, reg.NodeID)

	// a node that does not report: the checks turn critical after three
	// intervals
	now = now.Add(20 * time.Second)
	tick()
	must.Eq(t, structs.ServiceHealthPassing, getRegistration(t, s, "portal").Health)
	now = now.Add(20 * time.Second)
	tick()
	reg = getRegistration(t, s, "portal")
	must.Eq(t, structs.ServiceHealthCritical, reg.Health)
	must.Eq(t, externalCheckerOutputNoReport, reg.CheckState("http").Output)

	// and recover with its report
	_, err = e.updateChecks(structs.DefaultNamespace, "portal",
		[]*structs.ExternalCheckUpdate{{CheckID: "http", Status: structs.ServiceHealthPassing}}, other)
	must.NoError(t, err)
	must.Eq(t, structs.ServiceHealthPassing, getRegistration(t, s, "portal").Health)

	// no node at all: the checks are critical until one is there
	_, err = upsertExternal(t, s, declaredHTTPService("lonely", &structs.ExternalCheckedBy{NodePool: "nowhere"}), "", "")
	must.NoError(t, err)
	reg = getRegistration(t, s, "lonely")
	must.Eq(t, "", reg.NodeID)
	must.Eq(t, structs.ServiceHealthCritical, reg.Health)
	must.Eq(t, externalCheckerOutputNoNode, reg.CheckState("http").Output)

	// a service that is not declared with checked_by is deleted with its node
	must.NoError(t, s.State().DeleteServiceRegistrationByNodeID(structs.MsgTypeTestSetup, 1030, other))
	must.NotNil(t, getRegistration(t, s, "portal"))
}

func TestServiceRegistration_External_CheckedByValidation(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	// checked_by needs a check that a node runs
	reg := ttlService("ttl-only", time.Minute)
	reg.External.CheckedBy = &structs.ExternalCheckedBy{}
	_, err := upsertExternal(t, s, reg, "", "")
	must.ErrorContains(t, err, "checked_by needs an http or tcp check")

	// the Consul source is for the agent endpoints of the compat layer
	reg = declaredHTTPService("fake", &structs.ExternalCheckedBy{})
	reg.External.Source = structs.ExternalServiceSourceConsul
	_, err = upsertExternal(t, s, reg, "", "")
	must.ErrorContains(t, err, "invalid source")

	// a malformed datacenter pattern
	_, err = upsertExternal(t, s, declaredHTTPService("bad", &structs.ExternalCheckedBy{Datacenters: []string{"["}}), "", "")
	must.ErrorContains(t, err, "invalid datacenter pattern")
}
