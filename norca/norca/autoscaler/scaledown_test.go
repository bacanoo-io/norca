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

// downGroup is a group of three ready nodes with a scale_down block.
func downGroup(name, dc string, block map[string]any) testGroup {
	if block == nil {
		block = map[string]any{"unneeded_for": "5m", "delay_after_add": "10m", "utilization_below": float64(40)}
	}
	return testGroup{
		name: name, dc: dc,
		spec:   map[string]any{"replicas": float64(3), "min": float64(1), "policy": map[string]any{"scale_down": block}},
		status: map[string]any{"replicas": float64(3)},
	}
}

func vetoCondition(retryAfter time.Time) []any {
	return []any{map[string]any{
		"type": condScaleDownBlocked, "status": "True", "reason": "NoCandidate",
		"message": "every node holds work that can't be placed again", "retryAfter": retryAfter.Format(time.RFC3339),
	}}
}

func TestScaleDown_AfterUnneededFor(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	// Three nodes, 20% allocated: 30% without one node, below 40%.
	h, objects, _ := demandHarness(t, []testGroup{downGroup("workers-a", "dc-a", nil)}, nil)
	gauges(h, 3, 20)

	must.NoError(t, h.runner.scaleForDemand(ctx)) // starts the wait
	h.clock.Advance(4 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Len(t, 0, objects.requests())

	h.clock.Advance(2 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 2}, scaledTo(objects))

	// The operator has not removed the node yet (status.replicas 3, spec
	// 2): no further scale down.
	h.clock.Advance(time.Hour)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Len(t, 1, objects.requests())
}

func TestScaleDown_NotWhenNeeded(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	run := func(t *testing.T, groups []testGroup, work map[[2]string][]Unplaced, ready, allocated float64) *fakeObjectScaler {
		h, objects, _ := demandHarness(t, groups, work)
		gauges(h, ready, allocated)
		for range 3 {
			must.NoError(t, h.runner.scaleForDemand(ctx))
			h.clock.Advance(20 * time.Minute)
		}
		return objects
	}
	down := func(objects *fakeObjectScaler) bool {
		for _, r := range objects.requests() {
			if r.Replicas < 3 {
				return true
			}
		}
		return false
	}

	t.Run("busy", func(t *testing.T) {
		// 30% of three nodes is 45% of two.
		must.False(t, down(run(t, []testGroup{downGroup("workers-a", "dc-a", nil)}, nil, 3, 30)))
	})
	t.Run("no scale_down block", func(t *testing.T) {
		g := downGroup("workers-a", "dc-a", nil)
		g.spec["policy"] = map[string]any{}
		must.False(t, down(run(t, []testGroup{g}, nil, 3, 0)))
	})
	t.Run("at min", func(t *testing.T) {
		g := downGroup("workers-a", "dc-a", nil)
		g.spec["min"] = float64(3)
		must.False(t, down(run(t, []testGroup{g}, nil, 3, 0)))
	})
	t.Run("nodes joining", func(t *testing.T) {
		g := downGroup("workers-a", "dc-a", nil)
		g.status["joining"] = float64(1)
		must.False(t, down(run(t, []testGroup{g}, nil, 3, 0)))
	})
	t.Run("blocked work can use it", func(t *testing.T) {
		// Work that is blocked by something else than the size (it fits a
		// node on its way) still holds the group.
		g := downGroup("workers-a", "dc-a", nil)
		g.spec["max"] = float64(3)
		must.False(t, down(run(t, []testGroup{g}, bigWork(1, "dc-a"), 3, 0)))
	})
	t.Run("headroom", func(t *testing.T) {
		// Three nodes, 0% allocated: two free without one node; three wanted.
		g := downGroup("workers-a", "dc-a", nil)
		g.spec["headroom_nodes"] = float64(3)
		must.False(t, down(run(t, []testGroup{g}, nil, 3, 0)))
	})
	t.Run("gauges unknown", func(t *testing.T) {
		h, objects, _ := demandHarness(t, []testGroup{downGroup("workers-a", "dc-a", nil)}, nil)
		h.deps.GaugeHistory = func(string, map[string]string, time.Time, time.Time) (float64, float64, int, error) {
			return 0, 0, 0, nil
		}
		h.newRunner()
		for range 3 {
			must.NoError(t, h.runner.scaleForDemand(ctx))
			h.clock.Advance(20 * time.Minute)
		}
		must.False(t, down(objects))
	})
	t.Run("a single busy node", func(t *testing.T) {
		g := downGroup("workers-a", "dc-a", nil)
		g.spec["replicas"], g.spec["min"], g.status["replicas"] = float64(1), float64(0), float64(1)
		objects := run(t, []testGroup{g}, nil, 1, 5)
		must.Len(t, 0, objects.requests())
	})
}

func TestScaleDown_EmptySingleNode(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	// One empty node of a group with min 0 may go.
	g := downGroup("workers-a", "dc-a", nil)
	g.spec["replicas"], g.spec["min"], g.status["replicas"] = float64(1), float64(0), float64(1)
	h, objects, _ := demandHarness(t, []testGroup{g}, nil)
	gauges(h, 1, 0)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 0}, scaledTo(objects))
}

func TestScaleDown_TimerResets(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	h, objects, _ := demandHarness(t, []testGroup{downGroup("workers-a", "dc-a", nil)}, nil)

	gauges(h, 3, 20)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(4 * time.Minute)

	// The load comes back for a moment: the wait starts again.
	unneeded := h.runner.unneededSince
	gauges(h, 3, 60)
	h.runner.unneededSince = unneeded
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.MapEmpty(t, h.runner.unneededSince)

	gauges(h, 3, 20)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(4 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Len(t, 0, objects.requests())
	h.clock.Advance(2 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Len(t, 1, objects.requests())
}

func TestScaleDown_DelayAfterAdd(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	g := downGroup("workers-a", "dc-a", nil)
	g.spec["replicas"] = float64(2)
	h, objects, _ := demandHarness(t, []testGroup{g}, nil)
	gauges(h, 3, 20)

	// The scale was raised to 3 just now; status.replicas is 3.
	_, err := objects.ScaleObject(&structs.CustomObjectScaleRequest{
		Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Name: "workers-a", Replicas: 3,
	})
	must.NoError(t, err)

	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(6 * time.Minute) // unneeded long enough, but raised 6 minutes ago
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Len(t, 1, objects.requests())

	h.clock.Advance(5 * time.Minute) // 11 minutes after the raise: the wait starts
	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 2}, scaledTo(objects))
}

func TestScaleDown_Veto(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	// A veto until a time in the future holds the scale down; after it the
	// wait starts again.
	g := downGroup("workers-a", "dc-a", nil)
	h, objects, _ := demandHarness(t, []testGroup{g}, nil)
	gauges(h, 3, 20)
	obj, err := h.store.CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", "workers-a")
	must.NoError(t, err)
	obj = obj.Copy()
	obj.Status["conditions"] = vetoCondition(h.clock.Now().Add(30 * time.Minute))
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, 9000, obj, obj.ModifyIndex))

	for range 3 {
		must.NoError(t, h.runner.scaleForDemand(ctx))
		h.clock.Advance(10 * time.Minute)
	}
	must.Len(t, 0, objects.requests())
	h.clock.Advance(time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx)) // the wait starts
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 2}, scaledTo(objects))

	// A scale down that was decided and then vetoed is set back to the
	// nodes the group has.
	g = downGroup("workers-a", "dc-a", nil)
	g.spec["replicas"] = float64(2)
	g.status["conditions"] = vetoCondition(time.Now().Add(time.Hour))
	h, objects, _ = demandHarness(t, []testGroup{g}, nil)
	gauges(h, 3, 20)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-a": 3}, scaledTo(objects))
}

func TestScaleDown_OnePerPool(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	// Two groups of the pool may lose a node; the one with more nodes goes
	// first, and only one per look.
	a := downGroup("workers-a", "dc-a", nil)
	b := downGroup("workers-b", "dc-b", nil)
	b.spec["replicas"], b.status["replicas"] = float64(4), float64(4)
	h, objects, _ := demandHarness(t, []testGroup{a, b}, nil)
	gauges(h, 3, 10)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, map[string]int64{"workers-b": 3}, scaledTo(objects))

	// While the node of workers-b is being removed, the pool waits.
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Len(t, 1, objects.requests())
}

func TestScaleDown_DryRun(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	g := downGroup("workers-a", "dc-a", nil)
	g.spec["enforce"] = false
	h, objects, _ := demandHarness(t, []testGroup{g}, nil)
	gauges(h, 3, 20)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	for range 3 {
		h.clock.Advance(6 * time.Minute)
		must.NoError(t, h.runner.scaleForDemand(ctx))
	}
	must.Len(t, 0, objects.requests())

	// Said once, not on every look.
	log, err := h.store.ObjectEventLog(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", "workers-a")
	must.NoError(t, err)
	must.NotNil(t, log)
	dry := 0
	for _, e := range log.Events {
		if e.Reason == objectReasonDryRun {
			dry += int(e.Count)
		}
	}
	must.Eq(t, 1, dry)
}

// TestScaleDown_ChecksOnlyRaise checks that a check of the policy of a node
// group that would lower the scale is ignored: the scale_down block decides.
func TestScaleDown_ChecksOnlyRaise(t *testing.T) {
	ci.Parallel(t)
	spec := nodeGroupSpec()
	spec["enforce"] = true
	h, objects := nodeGroupHarness(t, spec)
	h.apm.value = 10 // a fifth of the target of 50: the check would lower it
	must.NoError(t, evaluateNodeGroup(t, h))
	must.Len(t, 0, objects.requests())
}
