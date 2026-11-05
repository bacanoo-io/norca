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

func nodeGroupHarness(t *testing.T, spec map[string]any) (*harness, *fakeObjectScaler) {
	h := newHarness(t, structs.ScalingPolicyEngineExternal, targetValuePolicy(nil), nil)
	objects := &fakeObjectScaler{t: t, store: h.store, clock: h.clock, index: 5000}
	h.deps.ObjectScaler = objects

	must.NoError(t, h.store.UpsertObjectKind(structs.MsgTypeTestSetup, 4000, &structs.ObjectKind{
		Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Schema: `{"type":"object"}`,
		Scale: &structs.ObjectKindScale{SpecPath: "spec.replicas", StatusPath: "status.replicas"},
	}))
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, 4001, &structs.CustomObject{
		ID: "ng-id", Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Name: "workers-dc-a",
		Generation: 1, Spec: spec,
	}, 0))
	h.newRunner()
	return h, objects
}

func nodeGroupSpec() map[string]any {
	return map[string]any{
		"pool": "workers", "datacenter": "dc-a", "min": float64(1), "max": float64(10),
		"replicas": float64(2), "provider": "lab", "engine": "builtin",
		"policy": map[string]any{
			"evaluation_interval": "30s",
			"cooldown":            "5m",
			"check": map[string]any{"allocated": map[string]any{
				"source": "fake", "query": "cpu", "query_window": "2m",
				"strategy": map[string]any{"target-value": map[string]any{"target": float64(50)}},
			}},
		},
	}
}

func evaluateNodeGroup(t *testing.T, h *harness) error {
	handle, ok := h.runner.handles[nodeGroupPolicyPrefix+"workers-dc-a"]
	must.True(t, ok)
	must.NotNil(t, handle.compiled)
	return h.runner.evaluate(context.Background(), handle.compiled)
}

func TestNodeGroupPolicy_Derive(t *testing.T) {
	ci.Parallel(t)

	obj := &structs.CustomObject{Name: "g", Spec: nodeGroupSpec()}
	obj.Spec["node_class"] = "gpu"
	p, err := nodeGroupPolicy(obj)
	must.NoError(t, err)
	must.Eq(t, nodeGroupPolicyPrefix+"g", p.ID)
	must.Eq(t, int64(1), p.Min)
	must.Eq(t, int64(10), p.Max)
	must.True(t, p.Enabled)
	must.Eq(t, "g", p.Target[structs.ScalingTargetNodeGroup])
	must.Eq(t, "workers", p.Target[TargetConfigKeyPool])
	must.Eq(t, "dc-a", p.Target[TargetConfigKeyDatacenter])
	must.Eq(t, "gpu", p.Target[TargetConfigKeyNodeClass])
	must.Eq(t, "true", p.Target["dry-run"]) // enforce is not set

	tp, err := TypedPolicy(p)
	must.NoError(t, err)
	must.Eq(t, 30*time.Second, tp.EvaluationInterval)
	must.Eq(t, 5*time.Minute, tp.Cooldown)
	must.Len(t, 1, tp.Checks)
	must.Eq(t, "allocated", tp.Checks[0].Name)
	must.Eq(t, 2*time.Minute, tp.Checks[0].QueryWindow)
	must.Eq(t, "target-value", tp.Checks[0].Strategy)

	// enforce, a disabled group and the external engine.
	obj.Spec["enforce"] = true
	p, _ = nodeGroupPolicy(obj)
	_, dry := p.Target["dry-run"]
	must.False(t, dry)
	obj.Spec["enabled"] = false
	p, _ = nodeGroupPolicy(obj)
	must.False(t, p.Enabled)
	obj.Spec["enabled"] = true
	obj.Spec["engine"] = "external"
	p, _ = nodeGroupPolicy(obj)
	must.False(t, p.Enabled)
}

func TestNodeGroupPolicy_Durations(t *testing.T) {
	ci.Parallel(t)
	for in, want := range map[string]time.Duration{
		"45d": 45 * 24 * time.Hour, "15m": 15 * time.Minute, "1h30m": 90 * time.Minute, "": 0,
	} {
		got, err := parseNodeDuration(in)
		must.NoError(t, err)
		must.Eq(t, want, got, must.Sprint(in))
	}
	_, err := parseNodeDuration("soon")
	must.Error(t, err)
}

// TestNodeGroupPolicy_DryRun checks that a group without enforce is
// evaluated and its decision recorded, and spec.replicas stays.
func TestNodeGroupPolicy_DryRun(t *testing.T) {
	ci.Parallel(t)
	h, objects := nodeGroupHarness(t, nodeGroupSpec())

	must.NoError(t, evaluateNodeGroup(t, h))
	must.Len(t, 0, objects.requests())
	obj, err := h.store.CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", "workers-dc-a")
	must.NoError(t, err)
	must.Eq[any](t, float64(2), obj.Spec["replicas"])

	log, err := h.store.ObjectEventLog(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", "workers-dc-a")
	must.NoError(t, err)
	must.NotNil(t, log)
	var dry bool
	for _, e := range log.Events {
		if e.Reason == objectReasonDryRun {
			dry = true
		}
	}
	must.True(t, dry)
}

// TestNodeGroupPolicy_Enforce checks that the decision is written to
// spec.replicas, that the cooldown comes from the object's log, and that a
// group that is disabled afterwards is not acted on.
func TestNodeGroupPolicy_Enforce(t *testing.T) {
	ci.Parallel(t)
	spec := nodeGroupSpec()
	spec["enforce"] = true
	h, objects := nodeGroupHarness(t, spec)

	must.NoError(t, evaluateNodeGroup(t, h))
	reqs := objects.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(4), reqs[0].Replicas) // the fake metric is twice the target
	must.Eq(t, "workers-dc-a", reqs[0].Name)
	must.Eq(t, structs.NodeScalingGroup, reqs[0].Group)
	must.Len(t, 0, h.scaler.requests()) // no task group was scaled

	// A new leader respects the cooldown that the object's log records.
	h.newRunner()
	h.clock.Advance(time.Minute)
	must.NoError(t, evaluateNodeGroup(t, h))
	must.Len(t, 1, objects.requests())
}

func TestNodeGroupPolicy_NotPicked(t *testing.T) {
	ci.Parallel(t)
	spec := nodeGroupSpec()
	spec["enabled"] = false
	h, _ := nodeGroupHarness(t, spec)
	_, ok := h.runner.handles[nodeGroupPolicyPrefix+"workers-dc-a"]
	must.False(t, ok)
}
