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

// A veto of the operator whose retryAfter is past does not set a scale down
// back: before, the group went 3 2 3 3 3 2 3 for good, the scale down never
// took effect (review finding ASC-2).
func TestScaleDown_ExpiredVetoDoesNotRevert(t *testing.T) {
	ci.Parallel(t)

	g := downGroup("workers-a", "dc-a", nil)
	h, objects, _ := demandHarness(t, []testGroup{g}, nil)
	gauges(h, 3, 20)
	obj, err := h.store.CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", "workers-a")
	must.NoError(t, err)
	obj = obj.Copy()
	obj.Status["conditions"] = vetoCondition(h.clock.Now().Add(-time.Hour))
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, 9000, obj, obj.ModifyIndex))

	// Looks every 30 s for 40 minutes; the operator leaves the status alone.
	for range 80 {
		must.NoError(t, h.runner.scaleForDemand(context.Background()))
		h.clock.Advance(30 * time.Second)
	}
	reqs := objects.requests()
	must.SliceNotEmpty(t, reqs, must.Sprint("the expired veto held the scale down"))
	for i := 1; i < len(reqs); i++ {
		must.False(t, reqs[i-1].Replicas < reqs[i].Replicas,
			must.Sprintf("the scale down was set back: %d then %d", reqs[i-1].Replicas, reqs[i].Replicas))
	}
}
