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

// reserveGroup is a healthy group of a pool with ready nodes, of which used
// carry work.
func reserveGroup(name, dc string, ready, used float64) *demandGroup {
	return &demandGroup{
		name: name, dc: dc, pool: "workers", weight: 1,
		freeNodes: ready, free: ready - used, freeKnown: true,
		size: NodeSize{CPU: 4000, Memory: 8000}, sizeKnown: true,
	}
}

func TestSetPoolReserve(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		groups   func() []*demandGroup
		expected map[string]float64
		lost     map[string]string
	}{
		{
			name: "three even datacenters keep half of one each",
			groups: func() []*demandGroup {
				return []*demandGroup{
					reserveGroup("a", "dc-a", 6, 4), reserveGroup("b", "dc-b", 6, 4), reserveGroup("c", "dc-c", 6, 4),
				}
			},
			expected: map[string]float64{"a": 2, "b": 2, "c": 2},
		},
		{
			name: "two datacenters keep all of the other",
			groups: func() []*demandGroup {
				return []*demandGroup{reserveGroup("a", "dc-a", 6, 5), reserveGroup("b", "dc-b", 6, 2)}
			},
			expected: map[string]float64{"a": 2, "b": 5},
			lost:     map[string]string{"a": "dc-b", "b": "dc-a"},
		},
		{
			name: "the datacenter that carries the most decides, and is not its own reserve",
			groups: func() []*demandGroup {
				return []*demandGroup{
					reserveGroup("a", "dc-a", 10, 8), reserveGroup("b", "dc-b", 4, 2), reserveGroup("c", "dc-c", 4, 2),
				}
			},
			expected: map[string]float64{"a": 1, "b": 4, "c": 4},
			lost:     map[string]string{"a": "dc-b", "b": "dc-a", "c": "dc-a"},
		},
		{
			name: "the work of the lost datacenter is split by the weights",
			groups: func() []*demandGroup {
				a, b, c := reserveGroup("a", "dc-a", 6, 6), reserveGroup("b", "dc-b", 6, 0), reserveGroup("c", "dc-c", 6, 0)
				b.weight = 2
				return []*demandGroup{a, b, c}
			},
			expected: map[string]float64{"a": 0, "b": 4, "c": 2},
		},
		{
			name: "two groups of a datacenter are lost together",
			groups: func() []*demandGroup {
				return []*demandGroup{
					reserveGroup("a1", "dc-a", 3, 2), reserveGroup("a2", "dc-a", 3, 2), reserveGroup("b", "dc-b", 6, 1),
				}
			},
			expected: map[string]float64{"a1": 0.5, "a2": 0.5, "b": 4},
		},
		{
			name: "smaller nodes stand in with more of them",
			groups: func() []*demandGroup {
				a, b := reserveGroup("a", "dc-a", 4, 3), reserveGroup("b", "dc-b", 8, 0)
				b.size = NodeSize{CPU: 2000, Memory: 4000}
				return []*demandGroup{a, b}
			},
			expected: map[string]float64{"a": 0, "b": 6},
		},
		{
			name: "without the size of a node the work is counted in nodes",
			groups: func() []*demandGroup {
				a, b := reserveGroup("a", "dc-a", 4, 3), reserveGroup("b", "dc-b", 8, 0)
				b.size, b.sizeKnown = NodeSize{}, false
				return []*demandGroup{a, b}
			},
			expected: map[string]float64{"a": 0, "b": 3},
		},
		{
			name: "a pool in one datacenter keeps none",
			groups: func() []*demandGroup {
				return []*demandGroup{reserveGroup("a1", "dc-a", 6, 4), reserveGroup("a2", "dc-a", 6, 4)}
			},
			expected: map[string]float64{"a1": 0, "a2": 0},
		},
		{
			name: "none while a group is not healthy: the reserve is in use",
			groups: func() []*demandGroup {
				a, b, c := reserveGroup("a", "dc-a", 6, 6), reserveGroup("b", "dc-b", 6, 6), reserveGroup("c", "dc-c", 0, 0)
				c.degraded = "no node of 6 is ready"
				return []*demandGroup{a, b, c}
			},
			expected: map[string]float64{"a": 0, "b": 0, "c": 0},
		},
		{
			name: "none when the use of a group can't be read",
			groups: func() []*demandGroup {
				a, b := reserveGroup("a", "dc-a", 6, 4), reserveGroup("b", "dc-b", 6, 4)
				b.freeKnown = false
				return []*demandGroup{a, b}
			},
			expected: map[string]float64{"a": 0, "b": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := tt.groups()
			setPoolReserve(groups)

			got := map[string]float64{}
			for _, g := range groups {
				got[g.name] = g.reserve
				if lost, ok := tt.lost[g.name]; ok {
					must.Eq(t, lost, g.reserveFor)
				}
			}
			must.Eq(t, tt.expected, got)
		})
	}
}

// A pool with the reserve raises its groups ahead of the loss, and only that
// pool does.
func TestDemand_ReserveForDatacenterLoss(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	// Three datacenters of four nodes, three of them used in each: one node
	// free, and half of another datacenter's three wanted (1.5).
	groups := func() []testGroup {
		var res []testGroup
		for _, dc := range []string{"a", "b", "c"} {
			res = append(res, testGroup{
				name: "workers-" + dc, dc: "dc-" + dc,
				spec:   map[string]any{"replicas": float64(4)},
				status: map[string]any{"replicas": float64(4)},
			})
		}
		return res
	}
	use := map[string]dcUse{"dc-a": {ready: 4, allocated: 75}, "dc-b": {ready: 4, allocated: 75}, "dc-c": {ready: 4, allocated: 75}}

	t.Run("off by default", func(t *testing.T) {
		h, objects, _ := demandHarness(t, groups(), nil)
		gaugesByDC(h, use)
		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Len(t, 0, objects.requests())
	})

	t.Run("every group is raised to hold its share", func(t *testing.T) {
		h, objects, _ := demandHarness(t, groups(), nil)
		h.cfg.NodeScaling = &NodeScalingConfig{Pools: map[string]*NodePoolScaling{"workers": {ReserveDatacenterLoss: true}}}
		gaugesByDC(h, use)
		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Eq(t, map[string]int64{"workers-a": 5, "workers-b": 5, "workers-c": 5}, scaledTo(objects))

		for _, req := range objects.requests() {
			must.StrContains(t, req.Message, "reserve for the loss of datacenter dc-")
			must.StrContains(t, req.Message, "1.0 nodes free (and 0 on their way), 1.5 wanted")
		}
	})

	t.Run("the pool maximum holds the reserve", func(t *testing.T) {
		h, objects, events := demandHarness(t, groups(), nil)
		h.cfg.NodeScaling = &NodeScalingConfig{Pools: map[string]*NodePoolScaling{
			"workers": {ReserveDatacenterLoss: true, MaxNodes: 13},
		}}
		gaugesByDC(h, use)
		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Len(t, 1, objects.requests())
		must.Positive(t, events.count(structs.TypeScalingNodeBlocked))
	})
}

// The scale down does not take the reserve: a group that would be unneeded
// by its use is kept when the pool keeps a reserve.
func TestScaleDown_KeepsTheReserve(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	// Two datacenters of four nodes, 25% used: one node of work each, three
	// free. Without one node two are free: more than the one the other
	// datacenter's work needs, so a node may go; with 50% used (two nodes of
	// work) one free node is left without one, and two are wanted.
	run := func(t *testing.T, allocated float64, reserve bool) map[string]int64 {
		groups := []testGroup{sizedDownGroup("workers-a", "dc-a", 4), sizedDownGroup("workers-b", "dc-b", 4)}
		for _, g := range groups {
			g.spec["policy"] = map[string]any{"scale_down": map[string]any{
				"unneeded_for": "5m", "delay_after_add": "10m", "utilization_below": float64(80),
			}}
		}
		h, objects, _ := demandHarness(t, groups, nil)
		h.cfg.NodeScaling = &NodeScalingConfig{Pools: map[string]*NodePoolScaling{"workers": {ReserveDatacenterLoss: reserve}}}
		gaugesByDC(h, map[string]dcUse{"dc-a": {ready: 4, allocated: allocated}, "dc-b": {ready: 4, allocated: allocated}})
		must.NoError(t, h.runner.scaleForDemand(ctx)) // starts the wait
		h.clock.Advance(11 * time.Minute)
		must.NoError(t, h.runner.scaleForDemand(ctx))
		return scaledTo(objects)
	}

	t.Run("a node the reserve does not need goes", func(t *testing.T) {
		must.Eq(t, map[string]int64{"workers-a": 3}, run(t, 25, true))
	})
	t.Run("a node of the reserve stays", func(t *testing.T) {
		must.MapEmpty(t, run(t, 50, true))
	})
	t.Run("without the reserve it goes", func(t *testing.T) {
		must.Eq(t, map[string]int64{"workers-a": 3}, run(t, 50, false))
	})
}
