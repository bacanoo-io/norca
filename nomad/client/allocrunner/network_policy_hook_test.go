// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/allocrunner/netpolicy"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

type fakeNetSpec struct{ path string }

func (f fakeNetSpec) networkSpec() *drivers.NetworkIsolationSpec {
	return &drivers.NetworkIsolationSpec{Mode: drivers.NetIsolationModeGroup, Path: f.path}
}

// fakeNetPolicyRPC answers GetAllocRules from a queue; when the queue is
// empty it blocks like a blocking query until the next push.
type fakeNetPolicyRPC struct {
	mu      sync.Mutex
	queue   []*structs.NetworkPolicyAllocResponse
	err     error
	calls   int
	pushed  chan struct{}
	lastReq structs.NetworkPolicyAllocRequest
}

func newFakeNetPolicyRPC() *fakeNetPolicyRPC {
	return &fakeNetPolicyRPC{pushed: make(chan struct{}, 100)}
}

func (f *fakeNetPolicyRPC) push(index uint64, rules *structs.NetworkPolicyAllocRules) {
	f.mu.Lock()
	resp := &structs.NetworkPolicyAllocResponse{Rules: rules}
	resp.Index = index
	f.queue = append(f.queue, resp)
	f.mu.Unlock()
	f.pushed <- struct{}{}
}

func (f *fakeNetPolicyRPC) RPC(method string, args, reply interface{}) error {
	if method != "NetworkPolicy.GetAllocRules" {
		return errors.New("unexpected method " + method)
	}
	req := args.(*structs.NetworkPolicyAllocRequest)
	for {
		f.mu.Lock()
		f.calls++
		f.lastReq = *req
		if f.err != nil {
			err := f.err
			f.mu.Unlock()
			return err
		}
		if len(f.queue) > 0 {
			resp := f.queue[0]
			f.queue = f.queue[1:]
			f.mu.Unlock()
			*reply.(*structs.NetworkPolicyAllocResponse) = *resp
			return nil
		}
		f.mu.Unlock()
		select {
		case <-f.pushed:
		case <-time.After(200 * time.Millisecond):
			// Simulate the end of a blocking query.
			r := reply.(*structs.NetworkPolicyAllocResponse)
			r.Rules = nil
			r.Index = req.MinQueryIndex
			return nil
		}
	}
}

type fakeNetPolicyBackend struct {
	mu       sync.Mutex
	applied  []*netpolicy.Ruleset
	applyErr error
	counts   map[string]uint64
}

func (b *fakeNetPolicyBackend) Apply(_ string, rs *netpolicy.Ruleset) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.applyErr != nil {
		return b.applyErr
	}
	b.applied = append(b.applied, rs)
	return nil
}

func (b *fakeNetPolicyBackend) ReadCounters(string) (map[string]uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts, nil
}

func (b *fakeNetPolicyBackend) Gateways(string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("172.26.64.1")}, nil
}

func (b *fakeNetPolicyBackend) appliedRulesets() []*netpolicy.Ruleset {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*netpolicy.Ruleset(nil), b.applied...)
}

type fakeTaskEvents struct {
	mu     sync.Mutex
	events []*structs.TaskEvent
}

func (f *fakeTaskEvents) EmitTaskEvents(e *structs.TaskEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func testNetPolicyAlloc(mode string) *structs.Allocation {
	alloc := mock.Alloc()
	if mode != "" {
		alloc.Job.TaskGroups[0].Networks = structs.Networks{{Mode: mode}}
	} else {
		alloc.Job.TaskGroups[0].Networks = nil
	}
	return alloc
}

func testNetPolicyRules(enforcement string) *structs.NetworkPolicyAllocRules {
	return &structs.NetworkPolicyAllocRules{Policies: []*structs.NetworkPolicy{{
		Name:      "egress",
		Namespace: structs.DefaultNamespace,
		Egress: []*structs.NetworkPolicyRule{{
			Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"10.0.0.0/8"}}},
			Ports: []string{"443/tcp"},
		}},
		Enforcement: enforcement,
	}}}
}

func newTestNetPolicyHook(t *testing.T, alloc *structs.Allocation, rpc *fakeNetPolicyRPC) (*networkPolicyHook, *fakeNetPolicyBackend, *fakeTaskEvents) {
	cfg := &config.Config{Region: "global", Node: mock.Node(), PublishAllocationMetrics: true}
	events := &fakeTaskEvents{}
	h := newNetworkPolicyHook(testlog.HCLogger(t), alloc, rpc, cfg, fakeNetSpec{path: "/var/run/netns/test"}, events)
	backend := &fakeNetPolicyBackend{}
	h.backend = backend
	t.Cleanup(h.stop)
	return h, backend, events
}

func TestNetworkPolicyHook_HostModeIsNoop(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeNetPolicyRPC()
	h, backend, _ := newTestNetPolicyHook(t, testNetPolicyAlloc(""), rpc)
	must.NoError(t, h.Prerun())
	must.NoError(t, h.Postrun())
	must.Eq(t, 0, rpc.calls)
	must.Len(t, 0, backend.appliedRulesets())
}

func TestNetworkPolicyHook_NoPoliciesNoRules(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeNetPolicyRPC()
	rpc.push(5, &structs.NetworkPolicyAllocRules{})
	h, backend, _ := newTestNetPolicyHook(t, testNetPolicyAlloc("bridge"), rpc)
	must.NoError(t, h.Prerun())
	time.Sleep(300 * time.Millisecond)
	must.NoError(t, h.Postrun())
	// Without policies nothing is written into the namespace.
	must.Len(t, 0, backend.appliedRulesets())
}

func TestNetworkPolicyHook_OldServers(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeNetPolicyRPC()
	rpc.err = errors.New("rpc: can't find service NetworkPolicy.GetAllocRules")
	h, backend, _ := newTestNetPolicyHook(t, testNetPolicyAlloc("bridge"), rpc)
	must.NoError(t, h.Prerun())
	must.NoError(t, h.Postrun())
	must.Len(t, 0, backend.appliedRulesets())
}

func TestNetworkPolicyHook_ApplyAndFollow(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeNetPolicyRPC()
	rpc.push(10, testNetPolicyRules(structs.NetworkPolicyEnforcementRequired))
	alloc := testNetPolicyAlloc("bridge")
	h, backend, _ := newTestNetPolicyHook(t, alloc, rpc)

	must.NoError(t, h.Prerun())
	applied := backend.appliedRulesets()
	must.Len(t, 1, applied)
	must.NotNil(t, applied[0].Egress)
	must.Nil(t, applied[0].Ingress)
	rpc.mu.Lock()
	must.Eq(t, alloc.ID, rpc.lastReq.AllocID)
	rpc.mu.Unlock()

	// The same rules are not applied again.
	rpc.push(11, testNetPolicyRules(structs.NetworkPolicyEnforcementRequired))
	// Removing the policy removes the table.
	rpc.push(12, &structs.NetworkPolicyAllocRules{})
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return len(backend.appliedRulesets()) == 2
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
	must.Nil(t, backend.appliedRulesets()[1])

	// The blocking query continues from the last index.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		rpc.mu.Lock()
		defer rpc.mu.Unlock()
		return rpc.lastReq.MinQueryIndex == 12
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))

	must.NoError(t, h.Postrun())
}

func TestNetworkPolicyHook_ApplyFailure(t *testing.T) {
	ci.Parallel(t)

	// Required enforcement fails the allocation.
	rpc := newFakeNetPolicyRPC()
	rpc.push(10, testNetPolicyRules(structs.NetworkPolicyEnforcementRequired))
	h, backend, events := newTestNetPolicyHook(t, testNetPolicyAlloc("bridge"), rpc)
	backend.applyErr = errors.New("no nftables")
	must.ErrorContains(t, h.Prerun(), "no nftables")
	must.Len(t, 1, events.events)
	must.Eq(t, TaskNetworkPolicyNotEnforced, events.events[0].Type)

	// Best effort only warns.
	rpc = newFakeNetPolicyRPC()
	rpc.push(10, testNetPolicyRules(structs.NetworkPolicyEnforcementBestEffort))
	h, backend, events = newTestNetPolicyHook(t, testNetPolicyAlloc("bridge"), rpc)
	backend.applyErr = errors.New("no nftables")
	must.NoError(t, h.Prerun())
	must.Len(t, 1, events.events)
	must.NoError(t, h.Postrun())
}

func TestNetworkPolicyHook_Counters(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeNetPolicyRPC()
	rpc.push(10, testNetPolicyRules(structs.NetworkPolicyEnforcementRequired))
	h, backend, _ := newTestNetPolicyHook(t, testNetPolicyAlloc("bridge"), rpc)
	must.NoError(t, h.Prerun())

	backend.mu.Lock()
	backend.counts = map[string]uint64{"egress_default": 7}
	backend.mu.Unlock()
	h.emitCounters("/var/run/netns/test")
	h.mu.Lock()
	must.Eq(t, uint64(7), h.counts["egress_default"])
	h.mu.Unlock()
	must.NoError(t, h.Postrun())
}
