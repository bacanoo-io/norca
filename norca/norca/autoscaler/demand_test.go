// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

type demandEvents struct {
	l      sync.Mutex
	events []structs.ScalingDecisionEvent
	types  []string
}

func (d *demandEvents) publish(typ string, ev *structs.ScalingDecisionEvent) {
	d.l.Lock()
	defer d.l.Unlock()
	d.types = append(d.types, typ)
	d.events = append(d.events, *ev)
}

func (d *demandEvents) count(typ string) int {
	d.l.Lock()
	defer d.l.Unlock()
	n := 0
	for _, t := range d.types {
		if t == typ {
			n++
		}
	}
	return n
}

type testGroup struct {
	name, dc string
	spec     map[string]any // overrides of the spec
	status   map[string]any
}

// demandHarness has node groups of the pool "workers", a node of 8000 MB
// and 4000 MHz, and the given blocked work.
func demandHarness(t *testing.T, groups []testGroup, work map[[2]string][]Unplaced) (*harness, *fakeObjectScaler, *demandEvents) {
	h := newHarness(t, structs.ScalingPolicyEngineExternal, targetValuePolicy(nil), nil)
	objects := &fakeObjectScaler{t: t, store: h.store, clock: h.clock, index: 5000}
	h.deps.ObjectScaler = objects
	events := &demandEvents{}
	h.deps.PublishEvent = events.publish
	h.deps.BlockedDemand = func() map[[2]string][]Unplaced { return work }

	must.NoError(t, h.store.UpsertObjectKind(structs.MsgTypeTestSetup, 4000, &structs.ObjectKind{
		Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Schema: `{"type":"object"}`,
		Scale: &structs.ObjectKindScale{SpecPath: "spec.replicas", StatusPath: "status.replicas"},
	}))
	for i, g := range groups {
		spec := map[string]any{
			"pool": "workers", "datacenter": g.dc, "min": float64(0), "max": float64(10),
			"replicas": float64(1), "provider": "lab", "engine": "builtin", "enforce": true,
		}
		for k, v := range g.spec {
			spec[k] = v
		}
		status := map[string]any{
			"replicas": float64(1), "joining": float64(0),
			"allocatable": map[string]any{"cpu": float64(4000), "memory": float64(8000), "disk": float64(20000)},
		}
		for k, v := range g.status {
			if v == nil {
				delete(status, k)
				continue
			}
			status[k] = v
		}
		must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, uint64(4001+i), &structs.CustomObject{
			ID: "ng-" + g.name, Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Name: g.name,
			Generation: 1, Spec: spec, Status: status,
		}, 0))
	}
	h.newRunner()
	return h, objects, events
}

func bigWork(count int, dcs ...string) map[[2]string][]Unplaced {
	work := map[[2]string][]Unplaced{}
	for _, dc := range dcs {
		work[[2]string{dc, "workers"}] = []Unplaced{{
			Namespace: "default", Job: "web", Group: "g", Count: count, CPU: 100, Memory: 6000,
		}}
	}
	return work
}

func scaledTo(objects *fakeObjectScaler) map[string]int64 {
	out := map[string]int64{}
	for _, r := range objects.requests() {
		out[r.Name] = r.Replicas
	}
	return out
}

func TestDemand_Spread(t *testing.T) {
	ci.Parallel(t)
	// Four allocations of 6 GB on nodes of 8 GB need four nodes; the work
	// can go to either datacenter and is split between the groups.
	h, objects, _ := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a"}, {name: "workers-b", dc: "dc-b"}},
		bigWork(4, "dc-a", "dc-b"))

	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 3, "workers-b": 3}, scaledTo(objects))
}

func TestDemand_Netting(t *testing.T) {
	ci.Parallel(t)
	// Two nodes are already on their way for two of the four nodes.
	h, objects, _ := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a", spec: map[string]any{"replicas": float64(3)},
			status: map[string]any{"replicas": float64(1), "joining": float64(2)}}},
		bigWork(4, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	// Four nodes needed, two asked for: two more.
	must.Eq(t, map[string]int64{"workers-a": 5}, scaledTo(objects))

	// Everything asked for: nothing more.
	h2, objects2, _ := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a", spec: map[string]any{"replicas": float64(5)},
			status: map[string]any{"replicas": float64(1), "joining": float64(4)}}},
		bigWork(4, "dc-a"))
	must.NoError(t, h2.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects2.requests())
}

func TestDemand_DryRun(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a", spec: map[string]any{"enforce": false}}},
		bigWork(2, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())

	log, err := h.store.ObjectEventLog(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", "workers-a")
	must.NoError(t, err)
	var dry int
	for _, e := range log.Events {
		if e.Reason == objectReasonDryRun {
			dry++
		}
	}
	must.Eq(t, 1, dry) // said once
}

func TestDemand_Limits(t *testing.T) {
	ci.Parallel(t)
	// A group at its maximum cannot grow.
	h, objects, events := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a", spec: map[string]any{"max": float64(1)}}},
		bigWork(2, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
	must.Eq(t, 1, events.count(structs.TypeScalingNodeBlocked))
	// ... and says so once.
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, 1, events.count(structs.TypeScalingNodeBlocked))

	// The step and the maximum of the pool.
	h, objects, events = demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a"}},
		bigWork(4, "dc-a"))
	h.cfg.NodeScaling = &NodeScalingConfig{Pools: map[string]*NodePoolScaling{"workers": {MaxStep: 2}}}
	h.deps.BlockedDemand = func() map[[2]string][]Unplaced { return bigWork(4, "dc-a") }
	h.newRunner()
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 3}, scaledTo(objects))
	must.Eq(t, 1, events.count(structs.TypeScalingNodeBlocked))

	h, objects, _ = demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a"}, {name: "workers-b", dc: "dc-b"}},
		bigWork(4, "dc-a", "dc-b"))
	h.cfg.NodeScaling = &NodeScalingConfig{Pools: map[string]*NodePoolScaling{"workers": {MaxNodes: 5}}}
	h.newRunner()
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	total := int64(0)
	for _, n := range scaledTo(objects) {
		total += n
	}
	// Two nodes exist (one each); the pool may have five.
	must.Eq(t, int64(5)-1, total-1) // the unscaled group keeps its 1
}

func TestDemand_Expanders(t *testing.T) {
	ci.Parallel(t)
	groups := []testGroup{
		{name: "workers-a", dc: "dc-a", spec: map[string]any{"priority": float64(1), "weight": float64(1)}},
		{name: "workers-b", dc: "dc-b", spec: map[string]any{"priority": float64(0), "weight": float64(3)}},
	}
	run := func(expander string) map[string]int64 {
		h, objects, _ := demandHarness(t, groups, bigWork(2, "dc-a", "dc-b"))
		h.cfg.NodeScaling = &NodeScalingConfig{Expander: expander}
		h.newRunner()
		must.NoError(t, h.runner.scaleForDemand(context.Background()))
		return scaledTo(objects)
	}
	// priority: the lower number takes it all.
	must.Eq(t, map[string]int64{"workers-b": 3}, run(ExpanderPriority))
	// spread: one each.
	must.Eq(t, map[string]int64{"workers-a": 2, "workers-b": 2}, run(ExpanderSpread))
	// weighted: the group with the larger weight takes both here.
	must.Eq(t, map[string]int64{"workers-b": 3}, run(ExpanderWeighted))
}

func TestDemand_Rules(t *testing.T) {
	ci.Parallel(t)

	// A distinct_property that used up the datacenter of one group: the
	// node goes to the group of the other.
	work := map[[2]string][]Unplaced{}
	for _, dc := range []string{"dc-a", "dc-b"} {
		work[[2]string{dc, "workers"}] = []Unplaced{{
			Namespace: "default", Job: "ha", Group: "g", Count: 1, CPU: 100, Memory: 1000,
			Distinct: []DistinctProperty{{Target: "${node.datacenter}", Limit: 1, Used: map[string]int{"dc-a": 1}}},
		}}
	}
	h, objects, _ := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a"}, {name: "workers-b", dc: "dc-b"}}, work)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-b": 2}, scaledTo(objects))

	// A static port: one node per allocation, however small.
	h, objects, _ = demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, map[[2]string][]Unplaced{
		{"dc-a", "workers"}: {{Namespace: "default", Job: "proxy", Group: "g", Count: 3, CPU: 50, Memory: 32, Exclusive: true}},
	})
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 4}, scaledTo(objects))

	// A constraint that can't be checked, an allocation that does not fit
	// a node, and a group whose node size is unknown.
	h, objects, events := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a"}, {name: "nosize", dc: "dc-c", status: map[string]any{"allocatable": nil}}},
		map[[2]string][]Unplaced{
			{"dc-a", "workers"}: {
				{Namespace: "default", Job: "odd", Group: "g", Count: 1, CPU: 1, Memory: 1, Unverifiable: "${meta.rack} distinct_property"},
				{Namespace: "default", Job: "huge", Group: "g", Count: 1, CPU: 1, Memory: 9000},
			},
			{"dc-c", "workers"}: {{Namespace: "default", Job: "lost", Group: "g", Count: 1, CPU: 1, Memory: 1}},
		})
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
	must.Eq(t, 1, events.count(structs.TypeScalingNodeUnverifiable))
	must.Eq(t, 1, events.count(structs.TypeScalingNodeAllocationTooLarge))
	must.Eq(t, 2, events.count(structs.TypeScalingNodeBlocked)) // huge: no group fits; lost: size unknown
}
