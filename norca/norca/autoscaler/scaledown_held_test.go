// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
	"golang.org/x/time/rate"
)

// A scale down of the balance that the executor held (here by the rate limit)
// is not a lowering: a raise of the group for blocked work after it does not
// switch the balance of the pool off (review finding ASC-6).
func TestScaleDown_HeldBalanceIsNotNoted(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	work := map[[2]string][]Unplaced{}
	h, objects, _ := demandHarness(t,
		[]testGroup{sizedDownGroup("workers-a", "dc-a", 8), sizedDownGroup("workers-b", "dc-b", 5)}, work)
	gaugesByDC(h, map[string]dcUse{"dc-a": {ready: 8, allocated: 90}, "dc-b": {ready: 5, allocated: 0}})

	limiter := h.runner.exec.limiter
	h.runner.exec.limiter = rate.NewLimiter(0, 0) // every action is dropped

	must.NoError(t, h.runner.scaleForDemand(ctx))
	h.clock.Advance(6 * time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.MapEmpty(t, scaledTo(objects))
	must.MapNotContainsKey(t, h.runner.balanceLowered, "workers")
	must.MapNotContainsKey(t, h.runner.lastDown, "workers-a")

	// Blocked work of dc-a raises the group; the balance stays on.
	h.runner.exec.limiter = limiter
	work[[2]string{"dc-a", "workers"}] = []Unplaced{{
		Namespace: "default", Job: "pinned", Group: "g", Count: 1, CPU: 100, Memory: 6000,
	}}
	h.clock.Advance(time.Minute)
	must.NoError(t, h.runner.scaleForDemand(ctx))
	must.Eq(t, int64(9), scaledTo(objects)["workers-a"])
	must.False(t, h.runner.balanceOffUntil["workers"].After(h.clock.Now()))
}
