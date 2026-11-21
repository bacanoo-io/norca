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

// specReplicas reads spec.replicas of a node group from the store.
func specReplicas(t *testing.T, h *harness, name string) int64 {
	t.Helper()
	obj, err := h.store.CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", name)
	must.NoError(t, err)
	return asInt(obj.Spec["replicas"])
}

// Work that stays blocked after the nodes asked for it joined (a check the
// new nodes fail as well, which the blocked evaluation did not show) does not
// grow the group again and again up to its maximum, nor keep it from scaling
// down, until its job changes (review finding ASC-1). Before, the group went
// 4 5 6 7 8 9 10 and stayed there.
func TestDemand_StuckWorkDoesNotGrowTheGroup(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	g := downGroup("workers-a", "dc-a", nil) // 3 ready, min 1, max 10, scale_down 5m/<40%
	work := map[[2]string][]Unplaced{{"dc-a", "workers"}: {{
		Namespace: "default", Job: "win", JobIndex: 7, Group: "g", Count: 1, CPU: 100, Memory: 256,
	}}}
	h, objects, _ := demandHarness(t, []testGroup{g}, work)
	gauges(h, 3, 0)
	key := targetKey{namespace: "default", job: "win", group: "g"}

	look := func() int64 {
		t.Helper()
		must.NoError(t, h.runner.scaleForDemand(ctx))
		n := specReplicas(t, h, "workers-a")
		// The operator brings up every node asked for before the next look.
		objects.setStatus("workers-a", map[string]any{"replicas": float64(n), "joining": float64(0)})
		return n
	}

	// The work raises the group by one node.
	must.Eq(t, int64(4), look())
	must.Eq(t, "", h.runner.capacity.unhelped(key))

	// The node joined a moment ago: the scheduler is still to try the work
	// on it, so nothing is asked for and the group keeps the node.
	h.clock.Advance(time.Minute)
	must.Eq(t, int64(4), look())

	// Still blocked well after the node joined: the work is stuck.
	var seq []int64
	for range 8 {
		h.clock.Advance(20 * time.Minute)
		seq = append(seq, look())
	}
	for _, n := range seq {
		must.LessEq(t, int64(4), n, must.Sprintf("the group grew for stuck work: %v", seq))
	}
	must.StrContains(t, h.runner.capacity.unhelped(key), "they joined, and the scheduler still cannot place it")
	// It does not keep the group from scaling down.
	must.Less(t, int64(4), seq[len(seq)-1], must.Sprintf("the group was not lowered: %v", seq))

	// A change of the job is new work: it may raise the group again.
	h.runner.deps.BlockedDemand = func() map[[2]string][]Unplaced {
		return map[[2]string][]Unplaced{{"dc-a", "workers"}: {{
			Namespace: "default", Job: "win", JobIndex: 8, Group: "g", Count: 1, CPU: 100, Memory: 256,
		}}}
	}
	before := specReplicas(t, h, "workers-a")
	h.clock.Advance(20 * time.Minute)
	must.Greater(t, before, look())
}

// Work that the scheduler found every node of a group ineligible for does not
// grow that group: a new node would be of the same class (review finding
// ASC-1).
func TestDemand_IneligibleGroupIsNotGrown(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	work := map[[2]string][]Unplaced{{"dc-a", "workers"}: {{
		Namespace: "default", Job: "win", Group: "g", Count: 1, CPU: 100, Memory: 256,
		IneligibleGroups: []string{"workers-a"},
	}}}
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, work)
	gauges(h, 3, 0)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.MapEmpty(t, scaledTo(objects))
	must.StrContains(t, h.runner.capacity.unhelped(targetKey{namespace: "default", job: "win", group: "g"}),
		"the scheduler found its nodes ineligible")
}
