// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// The evaluation of a node group's check raises its scale while the blocked
// work loop has a reading from before: the loop does not overwrite the raise
// with a value it computed from its old reading (review finding ASC-7: the
// writes were 10, then 6, and the event said "10 -> 6 up").
func TestExecuteObject_StaleDecisionIsNotWritten(t *testing.T) {
	ci.Parallel(t)

	policy := nodeGroupSpec()["policy"]
	g := testGroup{name: "workers-a", dc: "dc-a",
		spec:   map[string]any{"replicas": float64(5), "max": float64(20), "policy": policy},
		status: map[string]any{"replicas": float64(5)}}
	h, objects, _ := demandHarness(t, []testGroup{g}, nil)

	raced := false
	h.deps.BlockedDemand = func() map[[2]string][]Unplaced {
		// The loop has read the groups; the check raises the group now.
		if !raced {
			raced = true
			handle, ok := h.runner.handles[nodeGroupPolicyPrefix+"workers-a"]
			must.True(t, ok)
			must.NoError(t, h.runner.evaluate(context.Background(), handle.compiled))
		}
		return map[[2]string][]Unplaced{{"dc-a", "workers"}: {{
			Namespace: "default", Job: "web", Group: "g", Count: 1, CPU: 100, Memory: 6000,
		}}}
	}
	must.NoError(t, h.runner.scaleForDemand(context.Background()))

	var writes []int64
	for _, r := range objects.requests() {
		writes = append(writes, r.Replicas)
	}
	must.Eq(t, []int64{10}, writes)
	must.Eq(t, int64(10), specReplicas(t, h, "workers-a"))
}
