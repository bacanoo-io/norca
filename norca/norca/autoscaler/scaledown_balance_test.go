// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// dcUse is the ready nodes of a datacenter and how much of them is allocated,
// in percent.
type dcUse struct {
	ready, allocated float64
}

// gaugesByDC makes the cluster gauges of the harness answer by datacenter.
func gaugesByDC(h *harness, use map[string]dcUse) {
	h.deps.GaugeHistory = func(metric string, labels map[string]string, _, _ time.Time) (float64, float64, int, error) {
		u, ok := use[labels["dc"]]
		if !ok {
			return 0, 0, 0, nil
		}
		switch metric {
		case metricsstore.MetricClusterNodesReady:
			return u.ready, u.ready, 1, nil
		case metricsstore.MetricClusterCPUAllocated, metricsstore.MetricClusterMemoryAllocated:
			return u.allocated, u.allocated, 1, nil
		}
		return 0, 0, 0, nil
	}
	h.newRunner()
}

// sizedDownGroup is a group of n ready nodes with a scale_down block.
func sizedDownGroup(name, dc string, n int) testGroup {
	g := downGroup(name, dc, nil)
	g.spec["replicas"], g.status["replicas"] = float64(n), float64(n)
	return g
}

// After a datacenter was lost and came back, the pool has the nodes that
// were added to the other datacenter for its work, and its own, empty. The
// node that goes is one of the datacenter that grew, so the work moves back.
func TestScaleDown_Balance(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	// dc-a grew to eight nodes and carries the work; dc-b is back with its
	// five nodes, empty.
	groups := []testGroup{sizedDownGroup("workers-a", "dc-a", 8), sizedDownGroup("workers-b", "dc-b", 5)}
	use := map[string]dcUse{"dc-a": {ready: 8, allocated: 90}, "dc-b": {ready: 5, allocated: 0}}

	tests := []struct {
		name     string
		pool     *NodePoolScaling
		expected map[string]int64
	}{
		{
			name:     "the group above its share gives the node",
			expected: map[string]int64{"workers-a": 7},
		},
		{
			name:     "without the balance the unneeded group gives it",
			pool:     &NodePoolScaling{Balance: pointer.Of(false)},
			expected: map[string]int64{"workers-b": 4},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, objects, _ := demandHarness(t, groups, nil)
			if tt.pool != nil {
				h.cfg.NodeScaling = &NodeScalingConfig{Pools: map[string]*NodePoolScaling{"workers": tt.pool}}
			}
			gaugesByDC(h, use)

			must.NoError(t, h.runner.scaleForDemand(ctx)) // starts the wait
			h.clock.Advance(6 * time.Minute)
			must.NoError(t, h.runner.scaleForDemand(ctx))
			must.Eq(t, tt.expected, scaledTo(objects))
		})
	}
}

// A group that is not above its share by a whole node, or that may not be
// lowered, does not take the place of the unneeded group.
func TestScaleDown_BalanceLeavesTheUnneededGroup(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		groups []testGroup
		use    map[string]dcUse
	}{
		{
			name:   "the pool is in balance",
			groups: []testGroup{sizedDownGroup("workers-a", "dc-a", 5), sizedDownGroup("workers-b", "dc-b", 5)},
			use:    map[string]dcUse{"dc-a": {ready: 5, allocated: 90}, "dc-b": {ready: 5, allocated: 0}},
		},
		{
			name: "the group above its share has no scale_down block",
			groups: []testGroup{
				{name: "workers-a", dc: "dc-a", spec: map[string]any{"replicas": float64(8)}, status: map[string]any{"replicas": float64(8)}},
				sizedDownGroup("workers-b", "dc-b", 5),
			},
			use: map[string]dcUse{"dc-a": {ready: 8, allocated: 90}, "dc-b": {ready: 5, allocated: 0}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, objects, _ := demandHarness(t, tt.groups, nil)
			gaugesByDC(h, tt.use)

			must.NoError(t, h.runner.scaleForDemand(ctx))
			h.clock.Advance(6 * time.Minute)
			must.NoError(t, h.runner.scaleForDemand(ctx))
			must.Eq(t, map[string]int64{"workers-b": 4}, scaledTo(objects))
		})
	}
}

// The work of the group that was lowered for the balance could not move: it
// is blocked and raises the group again. The pool then goes without the
// balance for a while, so it is not lowered and raised in turns.
func TestScaleDown_BalanceSwitchedOffWhenTheWorkCannotMove(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	work := map[[2]string][]Unplaced{}
	h, objects, events := demandHarness(t,
		[]testGroup{sizedDownGroup("workers-a", "dc-a", 8), sizedDownGroup("workers-b", "dc-b", 5)}, work)
	gaugesByDC(h, map[string]dcUse{"dc-a": {ready: 8, allocated: 90}, "dc-b": {ready: 5, allocated: 0}})

	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 7}, scaledTo(objects))

	// The node is gone, and its allocation, bound to dc-a, is blocked.
	objects.setStatus("workers-a", map[string]any{"replicas": float64(7)})
	work[[2]string{"dc-a", "workers"}] = []Unplaced{{
		Namespace: "default", Job: "pinned", Group: "g", Count: 1, CPU: 100, Memory: 6000,
	}}
	h.clock.Advance(time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, int64(8), scaledTo(objects)["workers-a"])
	must.Eq(t, 1, events.countFor(structs.TypeScalingNodeBlocked, "workers"))
	must.True(t, h.runner.balanceOffUntil["workers"].After(h.clock.Now()))

	// The work is placed. From now the unneeded group itself is lowered.
	delete(work, [2]string{"dc-a", "workers"})
	objects.setStatus("workers-a", map[string]any{"replicas": float64(8)})
	h.clock.Advance(20 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, int64(4), scaledTo(objects)["workers-b"])
	must.Eq(t, int64(8), scaledTo(objects)["workers-a"])
}
