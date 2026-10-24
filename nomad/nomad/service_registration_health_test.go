// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeClock is a settable clock for the damper.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func testDamper(t *testing.T) (*serviceHealthDamper, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	srv := &Server{logger: testlog.HCLogger(t), config: &Config{Region: "global"}}
	d := newServiceHealthDamper(srv)
	d.now = clock.now
	return d, clock
}

func healthReg(health string) *structs.ServiceRegistration {
	return &structs.ServiceRegistration{
		ID:          "_nomad-task-a-group-web-http",
		ServiceName: "web",
		Namespace:   "default",
		NodeID:      "node",
		AllocID:     "a",
		JobID:       "job",
		Address:     "10.0.0.1",
		Port:        8080,
		Ready:       health != structs.ServiceHealthCritical,
		Health:      health,
	}
}

func TestServiceHealthDamper_Filter(t *testing.T) {
	ci.Parallel(t)

	d, clock := testDamper(t)
	state := map[string]*structs.ServiceRegistration{}
	lookup := func(_, id string) (*structs.ServiceRegistration, error) { return state[id], nil }
	write := func(regs []*structs.ServiceRegistration) {
		for _, reg := range regs {
			state[reg.ID] = reg.Copy()
		}
	}

	// a new registration is written with its health time
	out, err := d.filter([]*structs.ServiceRegistration{healthReg(structs.ServiceHealthPassing)}, lookup)
	must.NoError(t, err)
	must.Len(t, 1, out)
	must.Eq(t, clock.t.UnixNano(), out[0].HealthUpdatedAt)
	write(out)
	first := clock.t

	// the same registration again is not written
	out, err = d.filter([]*structs.ServiceRegistration{healthReg(structs.ServiceHealthPassing)}, lookup)
	must.NoError(t, err)
	must.Len(t, 0, out)

	// a health change within the window is held back
	clock.t = first.Add(time.Second)
	out, _ = d.filter([]*structs.ServiceRegistration{healthReg(structs.ServiceHealthCritical)}, lookup)
	must.Len(t, 0, out)
	must.MapLen(t, 1, d.pending)

	// flapping back merges it away
	clock.t = first.Add(2 * time.Second)
	out, _ = d.filter([]*structs.ServiceRegistration{healthReg(structs.ServiceHealthPassing)}, lookup)
	must.Len(t, 0, out)
	must.MapEmpty(t, d.pending)

	// a change held back is due once the window has passed
	clock.t = first.Add(3 * time.Second)
	_, _ = d.filter([]*structs.ServiceRegistration{healthReg(structs.ServiceHealthWarning)}, lookup)
	clock.t = first.Add(4 * time.Second)
	_, _ = d.filter([]*structs.ServiceRegistration{healthReg(structs.ServiceHealthCritical)}, lookup)
	must.Len(t, 0, d.due())
	clock.t = first.Add(5 * time.Second)
	due := d.due()
	must.Len(t, 1, due)
	must.Eq(t, structs.ServiceHealthCritical, due[0].Health, must.Sprint("the last change wins"))
	must.MapEmpty(t, d.pending)

	// a change after the window is written right away
	state[due[0].ID].Health = structs.ServiceHealthCritical
	state[due[0].ID].Ready = false
	clock.t = first.Add(20 * time.Second)
	out, _ = d.filter([]*structs.ServiceRegistration{healthReg(structs.ServiceHealthPassing)}, lookup)
	must.Len(t, 1, out)
	write(out)

	// a re-registration that changes more than the health is never held back
	clock.t = clock.t.Add(time.Second)
	moved := healthReg(structs.ServiceHealthCritical)
	moved.Port = 9090
	out, _ = d.filter([]*structs.ServiceRegistration{moved}, lookup)
	must.Len(t, 1, out)
}

// TestServiceRegistration_UpsertFlapping checks the flapping protection and
// the batching through the RPC endpoint: many changes of one instance within
// the window result in two Raft writes.
func TestServiceRegistration_UpsertFlapping(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	s.serviceHealth.window = 3 * time.Second

	node := mock.Node()
	node.Attributes["norca.version"] = "0.1.0"
	must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 10, node))

	upsert := func(health string) uint64 {
		reg := healthReg(health)
		reg.NodeID = node.ID
		req := &structs.ServiceRegistrationUpsertRequest{
			Services:     []*structs.ServiceRegistration{reg},
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: node.SecretID},
		}
		var resp structs.ServiceRegistrationUpsertResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationUpsertRPCMethod, req, &resp))
		return resp.Index
	}

	first := upsert(structs.ServiceHealthPassing)
	for i := 0; i < 10; i++ {
		upsert(structs.ServiceHealthCritical)
		upsert(structs.ServiceHealthPassing)
	}
	must.Eq(t, first, upsert(structs.ServiceHealthPassing), must.Sprint("no Raft write for unchanged or merged health"))

	upsert(structs.ServiceHealthCritical)
	reg, err := s.State().GetServiceRegistrationByID(nil, "default", healthReg("").ID)
	must.NoError(t, err)
	must.Eq(t, structs.ServiceHealthPassing, reg.Health, must.Sprint("the change is held back"))

	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			reg, _ := s.State().GetServiceRegistrationByID(nil, "default", healthReg("").ID)
			return reg != nil && reg.Health == structs.ServiceHealthCritical
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(50*time.Millisecond),
	))
	reg, _ = s.State().GetServiceRegistrationByID(nil, "default", healthReg("").ID)
	must.Greater(t, first, reg.ModifyIndex)
	must.Greater(t, reg.CreateIndex, reg.ModifyIndex)
	must.Positive(t, reg.HealthUpdatedAt)
}

func TestServiceRegistration_GetServiceHealthy(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	healthy := healthReg(structs.ServiceHealthPassing)
	critical := healthReg(structs.ServiceHealthCritical)
	critical.ID, critical.AllocID = "critical", "b"
	notReady := healthReg(structs.ServiceHealthCritical)
	notReady.ID, notReady.AllocID = "not-ready", "c"
	notReady.ServiceName = "db"
	notReady.Discovery = &structs.ServiceDiscovery{IncludeNotReady: true}
	legacy := &structs.ServiceRegistration{
		ID: "legacy", ServiceName: "web", Namespace: "default", AllocID: "d", JobID: "job", NodeID: "node", Address: "10.0.0.2", Ready: true,
	}
	must.NoError(t, s.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 20,
		[]*structs.ServiceRegistration{healthy, critical, notReady, legacy}))

	get := func(name string, healthyOnly bool) []string {
		req := &structs.ServiceRegistrationByNameRequest{
			ServiceName:  name,
			HealthyOnly:  healthyOnly,
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default"},
		}
		var resp structs.ServiceRegistrationByNameResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.ServiceRegistrationGetServiceRPCMethod, req, &resp))
		var ids []string
		for _, reg := range resp.Services {
			ids = append(ids, reg.ID)
		}
		return ids
	}

	must.SliceContainsAll(t, []string{healthy.ID, critical.ID, legacy.ID}, get("web", false))
	must.SliceContainsAll(t, []string{healthy.ID, legacy.ID}, get("web", true))
	must.Eq(t, []string{"not-ready"}, get("db", true))
}
