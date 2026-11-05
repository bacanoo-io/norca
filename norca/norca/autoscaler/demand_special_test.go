// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func workIn(pool string, dcs []string, u Unplaced) map[[2]string][]Unplaced {
	out := map[[2]string][]Unplaced{}
	for _, dc := range dcs {
		out[[2]string{dc, pool}] = []Unplaced{u}
	}
	return out
}

// Special cases of the blocked work trigger.

func TestDemandSpecial_PoolsAreSeparate(t *testing.T) {
	ci.Parallel(t)
	// The work is in the pool "edge"; the only group is in "workers".
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}},
		workIn("edge", []string{"dc-a"}, Unplaced{Namespace: "default", Job: "p", Group: "g", Count: 2, CPU: 10, Memory: 10}))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
}

func TestDemandSpecial_DisabledAndExternalGroups(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t, []testGroup{
		{name: "off", dc: "dc-a", spec: map[string]any{"enabled": false}},
		{name: "ext", dc: "dc-a", spec: map[string]any{"engine": "external"}},
	}, bigWork(2, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
}

func TestDemandSpecial_ReadyNodesUnknown(t *testing.T) {
	ci.Parallel(t)
	// No status of an operator and no gauges: the nodes on their way can't
	// be told, so the group is left alone, however much work waits.
	h, objects, events := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		status: map[string]any{"replicas": nil, "joining": nil}}}, bigWork(4, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
	must.Eq(t, 0, events.count(structs.TypeScalingNodeBlocked))

	// With a gauge it works; if the gauge then fails, the last count is used
	// and the work is not asked for again.
	gauges(h, 1, 10)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 1, objects.requests())
	first := scaledTo(objects)["workers-a"]

	h.deps.GaugeHistory = func(string, map[string]string, time.Time, time.Time) (float64, float64, int, error) {
		return 0, 0, 0, nil
	}
	// The runner keeps its memory of the last count when it is the same run.
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 1, objects.requests())
	must.Eq(t, first, scaledTo(objects)["workers-a"])
}

func TestDemandSpecial_DistinctPropertyCapsTheCount(t *testing.T) {
	ci.Parallel(t)
	// Five allocations are blocked, but the job may have two more in the
	// datacenter (limit three, one there): two nodes, not five.
	u := Unplaced{Namespace: "default", Job: "ha", Group: "g", Count: 5, CPU: 10, Memory: 10, Exclusive: true,
		Distinct: []DistinctProperty{{Target: "${node.datacenter}", Limit: 3, Used: map[string]int{"dc-a": 1}}}}
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, workIn("workers", []string{"dc-a"}, u))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 3}, scaledTo(objects)) // 1 + 2
}

func TestDemandSpecial_ExclusiveSharesNodeWithOthers(t *testing.T) {
	ci.Parallel(t)
	// A proxy with a static port (3 instances) and small apps that fit next
	// to them: three nodes, not four or six.
	work := map[[2]string][]Unplaced{{"dc-a", "workers"}: {
		{Namespace: "default", Job: "proxy", Group: "g", Count: 3, CPU: 50, Memory: 100, Exclusive: true},
		{Namespace: "default", Job: "app", Group: "g", Count: 3, CPU: 500, Memory: 1000},
	}}
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, work)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 4}, scaledTo(objects)) // 1 + 3
}

func TestDemandSpecial_PendingRoomWorksAtTheMaximum(t *testing.T) {
	ci.Parallel(t)
	// The group is at its maximum, but a node on its way has room for the
	// work: no growth is needed and nothing is said to be blocked.
	h, objects, events := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec:   map[string]any{"replicas": float64(3), "max": float64(3)},
		status: map[string]any{"replicas": float64(1), "joining": float64(2)}}}, bigWork(2, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
	must.Eq(t, 0, events.count(structs.TypeScalingNodeBlocked))
}

func TestDemandSpecial_FillPicksTheFullerGroup(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t, []testGroup{
		{name: "workers-a", dc: "dc-a", spec: map[string]any{"replicas": float64(1), "max": float64(10)}},
		{name: "workers-b", dc: "dc-b", spec: map[string]any{"replicas": float64(4), "max": float64(5)},
			status: map[string]any{"replicas": float64(4)}},
	}, bigWork(1, "dc-a", "dc-b"))
	h.cfg.NodeScaling = &NodeScalingConfig{Expander: ExpanderFill}
	h.newRunner()
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-b": 5}, scaledTo(objects)) // the fuller one, below its maximum
}

func TestDemandSpecial_PoolOverridesRegionExpander(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t, []testGroup{
		{name: "workers-a", dc: "dc-a", spec: map[string]any{"priority": float64(5)}},
		{name: "workers-b", dc: "dc-b", spec: map[string]any{"priority": float64(1)}},
	}, bigWork(1, "dc-a", "dc-b"))
	h.cfg.NodeScaling = &NodeScalingConfig{Expander: ExpanderSpread,
		Pools: map[string]*NodePoolScaling{"workers": {Expander: ExpanderPriority}}}
	h.newRunner()
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-b": 2}, scaledTo(objects))
}

func TestDemandSpecial_AllocationFitsOnlyOneGroup(t *testing.T) {
	ci.Parallel(t)
	// Group a has small nodes, b large ones: the allocation goes to b.
	h, objects, _ := demandHarness(t, []testGroup{
		{name: "workers-a", dc: "dc-a", status: map[string]any{"allocatable": map[string]any{"cpu": float64(500), "memory": float64(1000), "disk": float64(20000)}}},
		{name: "workers-b", dc: "dc-b"},
	}, bigWork(1, "dc-a", "dc-b"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-b": 2}, scaledTo(objects))
}

func TestDemandSpecial_ManyUnitsStayBounded(t *testing.T) {
	ci.Parallel(t)
	// A huge backlog is bounded by the maximum of the group, and the answer
	// comes in one decision.
	h, objects, events := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a", spec: map[string]any{"max": float64(8)}}},
		bigWork(100000, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 8}, scaledTo(objects))
	must.True(t, events.count(structs.TypeScalingNodeBlocked) >= 1)
}

func TestDemandSpecial_SameWorkInTwoDatacentersIsOneWork(t *testing.T) {
	ci.Parallel(t)
	items := collectDemandItems(map[[2]string][]Unplaced{
		{"dc-a", "workers"}: {{Namespace: "default", Job: "w", Group: "g", Count: 2}},
		{"dc-b", "workers"}: {{Namespace: "default", Job: "w", Group: "g", Count: 2}},
		{"dc-a", "edge"}:    {{Namespace: "default", Job: "w", Group: "g", Count: 2}},
	})
	must.Len(t, 2, items) // one for workers (two datacenters), one for edge
	var workers *demandItem
	for _, it := range items {
		if it.pool == "workers" {
			workers = it
		}
	}
	must.Eq(t, []string{"dc-a", "dc-b"}, workers.dcs)
}

// Special cases of the headroom.

func TestHeadroomSpecial_LimitedByTheMaximum(t *testing.T) {
	ci.Parallel(t)
	h, objects, events := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec: map[string]any{"headroom_nodes": float64(5), "max": float64(2)}}}, nil)
	gauges(h, 4, 100)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 2}, scaledTo(objects))
	must.Eq(t, 1, events.count(structs.TypeScalingNodeBlocked)) // says it is limited
}

func TestHeadroomSpecial_MemoryIsTheScarcerDimension(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec: map[string]any{"headroom_nodes": float64(2)}}}, nil)
	h.deps.GaugeHistory = gaugeFunc(4, 10, 90) // CPU 10%, memory 90%
	h.newRunner()
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	// 4 nodes, 90% allocated: 0.4 free, 2 wanted: 2 more (ceil of 1.6).
	must.Eq(t, map[string]int64{"workers-a": 3}, scaledTo(objects))
}

func TestHeadroomSpecial_DryRunIsSaidOnce(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec: map[string]any{"headroom_nodes": float64(2), "enforce": false}}}, nil)
	gauges(h, 4, 75)
	for i := 0; i < 3; i++ {
		must.NoError(t, h.runner.scaleForDemand(context.Background()))
	}
	must.Len(t, 0, objects.requests())
	log, err := h.store.ObjectEventLog(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", "workers-a")
	must.NoError(t, err)
	var dry int
	for _, e := range log.Events {
		if e.Reason == objectReasonDryRun {
			dry += int(e.Count)
		}
	}
	must.Eq(t, 1, dry)
}

func TestHeadroomSpecial_NoHeadroomNoGaugeRead(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, nil)
	var reads int
	h.deps.GaugeHistory = func(string, map[string]string, time.Time, time.Time) (float64, float64, int, error) {
		reads++
		return 1, 1, 1, nil
	}
	h.newRunner()
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
	// The status of the harness group has the replicas, so nothing needs the
	// gauges at all.
	must.Eq(t, 0, reads)
}

// gaugeFunc answers the three cluster gauges: ready nodes, CPU and memory
// allocated.
func gaugeFunc(ready, cpu, memory float64) func(string, map[string]string, time.Time, time.Time) (float64, float64, int, error) {
	return func(metric string, _ map[string]string, _, _ time.Time) (float64, float64, int, error) {
		switch metric {
		case "norca.cluster.nodes_ready":
			return ready, ready, 1, nil
		case "norca.cluster.cpu_allocated":
			return cpu, cpu, 1, nil
		case "norca.cluster.memory_allocated":
			return memory, memory, 1, nil
		}
		return 0, 0, 0, nil
	}
}

// Special cases of the derived policy.

func TestNodeGroupPolicySpecial_Durations(t *testing.T) {
	ci.Parallel(t)
	for in, want := range map[string]time.Duration{
		"0s": 0, "1d12h": 36 * time.Hour, "90m": 90 * time.Minute, "2h30m15s": 2*time.Hour + 30*time.Minute + 15*time.Second,
	} {
		got, err := parseNodeDuration(in)
		must.NoError(t, err, must.Sprint(in))
		must.Eq(t, want, got, must.Sprint(in))
	}
	for _, in := range []string{"1x", "-5m", "5", "m", "1h 30m", "1.5h"} {
		_, err := parseNodeDuration(in)
		must.Error(t, err, must.Sprint(in))
	}
}

func TestNodeGroupPolicySpecial_BadDurationIsAnError(t *testing.T) {
	ci.Parallel(t)
	spec := nodeGroupSpec()
	spec["policy"].(map[string]any)["cooldown"] = "soon"
	_, err := nodeGroupPolicy(&structs.CustomObject{Name: "g", Spec: spec})
	must.Error(t, err)
}

func TestNodeGroupPolicySpecial_NoPolicyBlock(t *testing.T) {
	ci.Parallel(t)
	// A group may have no checks at all; it is then scaled by the blocked
	// work and the headroom only.
	spec := nodeGroupSpec()
	delete(spec, "policy")
	p, err := nodeGroupPolicy(&structs.CustomObject{Name: "g", Spec: spec})
	must.NoError(t, err)
	must.True(t, p.Enabled)
	must.MapEmpty(t, p.Policy)

	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, bigWork(2, "dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 3}, scaledTo(objects))
}

func TestNodeGroupPolicySpecial_ManyChecks(t *testing.T) {
	ci.Parallel(t)
	spec := nodeGroupSpec()
	spec["policy"].(map[string]any)["check"] = map[string]any{
		"cpu": map[string]any{"source": "fake", "query": "cpu",
			"strategy": map[string]any{"target-value": map[string]any{"target": float64(50)}}},
		"mem": map[string]any{"source": "fake", "query": "mem", "on_error": "ignore",
			"strategy": map[string]any{"threshold": map[string]any{"upper_bound": float64(80), "delta": float64(1)}}},
	}
	p, err := nodeGroupPolicy(&structs.CustomObject{Name: "g", Spec: spec})
	must.NoError(t, err)
	tp, err := TypedPolicy(p)
	must.NoError(t, err)
	must.Len(t, 2, tp.Checks)
	names := map[string]string{}
	for _, c := range tp.Checks {
		names[c.Name] = c.Strategy
	}
	must.Eq(t, map[string]string{"cpu": "target-value", "mem": "threshold"}, names)
}

func TestNodeGroupPolicySpecial_RenamedGroupIsAnotherPolicy(t *testing.T) {
	ci.Parallel(t)
	a, _ := nodeGroupPolicy(&structs.CustomObject{Name: "a", Spec: nodeGroupSpec()})
	b, _ := nodeGroupPolicy(&structs.CustomObject{Name: "b", Spec: nodeGroupSpec()})
	must.NotEq(t, a.ID, b.ID)
	ka, _ := targetKeyFromPolicy(a)
	kb, _ := targetKeyFromPolicy(b)
	must.NotEq(t, ka, kb)
	must.Eq(t, "nodegroup/a", ka.String())
}

func TestDemandSpecial_DistinctPropertyRoomIsPerDatacenter(t *testing.T) {
	ci.Parallel(t)
	// Two datacenters, a limit of two per datacenter, one allocation in each:
	// each can take one more, so one node is asked for in each group, not one
	// in total and not three.
	u := Unplaced{Namespace: "default", Job: "ha", Group: "g", Count: 3, CPU: 10, Memory: 10, Exclusive: true,
		Distinct: []DistinctProperty{{Target: "${node.datacenter}", Limit: 2, Used: map[string]int{"dc-a": 1, "dc-b": 1}}}}
	h, objects, _ := demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a"}, {name: "workers-b", dc: "dc-b"}},
		workIn("workers", []string{"dc-a", "dc-b"}, u))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 2, "workers-b": 2}, scaledTo(objects))

	// With one datacenter used up, the other takes what it has room for.
	u.Distinct = []DistinctProperty{{Target: "${node.datacenter}", Limit: 2, Used: map[string]int{"dc-a": 2, "dc-b": 1}}}
	h, objects, _ = demandHarness(t,
		[]testGroup{{name: "workers-a", dc: "dc-a"}, {name: "workers-b", dc: "dc-b"}},
		workIn("workers", []string{"dc-a", "dc-b"}, u))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-b": 2}, scaledTo(objects))
}
