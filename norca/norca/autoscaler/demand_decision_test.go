// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// groupStatus reads the status of a node group of the harness.
func groupStatus(t *testing.T, h *harness, name string) map[string]any {
	obj, err := h.store.CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", name)
	must.NoError(t, err)
	must.NotNil(t, obj)
	return obj.Status
}

// evalWork is blocked work of two task groups from two evaluations.
func evalWork(dc string) map[[2]string][]Unplaced {
	return map[[2]string][]Unplaced{{dc, "workers"}: {
		{Namespace: "default", Job: "web", Group: "g", EvalID: "eval-b", Count: 2, CPU: 100, Memory: 6000},
		{Namespace: "default", Job: "api", Group: "g", EvalID: "eval-a", Count: 1, CPU: 100, Memory: 6000},
	}}
}

func TestDemandDecision_WrittenWithTheEvaluations(t *testing.T) {
	ci.Parallel(t)
	// Without status.joining the nodes on their way are spec less ready.
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		status: map[string]any{"joining": nil}}}, evalWork("dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, map[string]int64{"workers-a": 4}, scaledTo(objects))

	status := groupStatus(t, h, "workers-a")
	d := asMap(status[decisionKey])
	must.NotNil(t, d)
	must.Eq(t, 4.0, d["replicas"])
	must.Eq(t, 1.0, d["previous"])
	must.Eq(t, checkBlockedDemand, d["check"])
	must.Eq[any](t, []any{"eval-a", "eval-b"}, d["evaluations"])
	must.Eq[any](t, []any{
		map[string]any{"namespace": "default", "job": "api", "group": "g", "count": 1.0},
		map[string]any{"namespace": "default", "job": "web", "group": "g", "count": 2.0},
	}, d["work"])
	must.StrContains(t, d["reason"].(string), "blocked work needs 3 nodes")
	must.NotEq(t, "", d["time"])

	// The fields of the operator are kept.
	must.Eq(t, 1.0, status["replicas"])
	must.NotNil(t, status["allocatable"])

	// The same work at the next look: the nodes are on their way, nothing more
	// is asked for or written.
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 1, objects.requests())
	must.Eq(t, 1, objects.statusWrites)

	// The decision is there already: it is not written again.
	groups, err := h.runner.readDemandGroups(h.clock.Now())
	must.NoError(t, err)
	h.runner.recordDecision(groups[0], 4, checkBlockedDemand, "again")
	must.Eq(t, 1, objects.statusWrites)
}

func TestDemandDecision_NotForADryRun(t *testing.T) {
	ci.Parallel(t)
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec: map[string]any{"enforce": false}}}, evalWork("dc-a"))
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Len(t, 0, objects.requests())
	must.Eq(t, 0, objects.statusWrites)
	must.Nil(t, groupStatus(t, h, "workers-a")[decisionKey])
}

func TestDemandDecision_SettledWhenTheNodesAreReady(t *testing.T) {
	ci.Parallel(t)
	decision := map[string]any{"replicas": float64(3), "previous": float64(1), "check": checkBlockedDemand}

	// Two of the three nodes are ready: the decision stays.
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec:   map[string]any{"replicas": float64(3)},
		status: map[string]any{"replicas": float64(2), "joining": float64(1), decisionKey: decision}}}, nil)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, 0, objects.statusWrites)
	must.NotNil(t, groupStatus(t, h, "workers-a")[decisionKey])

	// All three are ready: the decision goes, the rest of the status stays.
	h, objects, _ = demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec:   map[string]any{"replicas": float64(3)},
		status: map[string]any{"replicas": float64(3), decisionKey: decision}}}, nil)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, 1, objects.statusWrites)
	status := groupStatus(t, h, "workers-a")
	must.Nil(t, status[decisionKey])
	must.Eq(t, 3.0, status["replicas"])

	// The scale was set below the decision by hand: it goes too.
	h, objects, _ = demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec:   map[string]any{"replicas": float64(2)},
		status: map[string]any{"replicas": float64(1), decisionKey: decision}}}, nil)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.Eq(t, 1, objects.statusWrites)
	must.Nil(t, groupStatus(t, h, "workers-a")[decisionKey])
}

func TestDemandDecision_HeadroomHasNoEvaluations(t *testing.T) {
	ci.Parallel(t)
	h, _, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a",
		spec: map[string]any{"headroom_nodes": float64(2)}}}, nil)
	gauges(h, 4, 100)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	d := asMap(groupStatus(t, h, "workers-a")[decisionKey])
	must.NotNil(t, d)
	must.Eq(t, checkHeadroom, d["check"])
	must.Nil(t, d["evaluations"])
}
