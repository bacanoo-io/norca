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

func TestCapacityBook(t *testing.T) {
	ci.Parallel(t)

	web := targetKey{namespace: "default", job: "shop", group: "web"}
	api := targetKey{namespace: "default", job: "shop", group: "api"}
	book := newCapacityBook()
	must.Eq(t, "", book.unhelped(web))

	book.replace(map[targetKey]string{web: "at its maximum"})
	must.Eq(t, "at its maximum", book.unhelped(web))
	must.Eq(t, "", book.unhelped(api))

	// The next look replaces everything the last one found.
	book.replace(map[targetKey]string{api: "too large"})
	must.Eq(t, "", book.unhelped(web))
	must.Eq(t, "too large", book.unhelped(api))
}

// The blocked work trigger notes the work that more nodes will not help.
func TestDemand_NotesUnhelpedWork(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	web := targetKey{namespace: "default", job: "web", group: "g"}

	tests := []struct {
		name     string
		groups   []testGroup
		work     map[[2]string][]Unplaced
		expected string // "" when the work is helped
	}{
		{
			name:     "work that gets its nodes",
			groups:   []testGroup{{name: "workers-a", dc: "dc-a"}},
			work:     bigWork(2, "dc-a"),
			expected: "",
		},
		{
			name:     "no node group at all",
			groups:   nil,
			work:     bigWork(2, "dc-a"),
			expected: "no node group adds nodes",
		},
		{
			name:     "no node group of its datacenter",
			groups:   []testGroup{{name: "workers-b", dc: "dc-b"}},
			work:     bigWork(2, "dc-a"),
			expected: "no node group can take it: no node group of its datacenters and node pool",
		},
		{
			name:     "the group is at its maximum",
			groups:   []testGroup{{name: "workers-a", dc: "dc-a", spec: map[string]any{"max": float64(1)}}},
			work:     bigWork(2, "dc-a"),
			expected: "every node group that could take it is at its maximum",
		},
		{
			name:     "the maximum leaves a part of the work without a node",
			groups:   []testGroup{{name: "workers-a", dc: "dc-a", spec: map[string]any{"max": float64(2)}}},
			work:     bigWork(4, "dc-a"),
			expected: "every node group that could take it is at its maximum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _ := demandHarness(t, tt.groups, tt.work)
			must.NoError(t, h.runner.scaleForDemand(ctx))
			must.Eq(t, tt.expected, h.runner.capacity.unhelped(web))
		})
	}

	t.Run("work that is helped later is no longer noted", func(t *testing.T) {
		work := bigWork(2, "dc-a")
		h, _, _ := demandHarness(t, nil, work)
		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.NotEq(t, "", h.runner.capacity.unhelped(web))

		delete(work, [2]string{"dc-a", "workers"})
		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Eq(t, "", h.runner.capacity.unhelped(web))
	})
}

// A group whose allocations have waited for capacity for the settle timeout,
// and for which none is coming, is not scaled up further. Until then, and
// when capacity is coming, scale ups are made.
func TestRunner_Evaluate_CapacityHold(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name            string
		isUnhelped      bool
		isDeploying     bool
		wait            time.Duration
		expectedOutcome string
		expectedHold    string
	}{
		{name: "no capacity is coming, after the settle timeout", isUnhelped: true, wait: 2 * time.Minute,
			expectedOutcome: OutcomeHeld, expectedHold: HoldCapacity},
		{name: "no capacity is coming, before the settle timeout", isUnhelped: true, wait: time.Minute,
			expectedOutcome: OutcomeScaled},
		{name: "capacity is coming", isUnhelped: false, wait: 2 * time.Minute,
			expectedOutcome: OutcomeScaled},
		// The allocations a scale up adds to a deployment are part of it,
		// and the deployment fails if they are never placed: no wait.
		{name: "no capacity is coming, during a deployment", isUnhelped: true, isDeploying: true, wait: time.Minute,
			expectedOutcome: OutcomeHeld, expectedHold: HoldCapacity},
		{name: "capacity is coming, during a deployment", isUnhelped: false, isDeploying: true, wait: time.Minute,
			expectedOutcome: OutcomeScaled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil),
				func(cfg *Config) { cfg.SettleTimeout = 2 * time.Minute })
			key := targetKey{namespace: h.job.Namespace, job: h.job.ID, group: "web"}

			// 2 of 4 run and show 150 against a target of 50: the load
			// needs 6. The other two wait for capacity.
			h.apm.value = 150
			h.setCount(4)
			h.addAlloc(structs.AllocClientStatusPending)
			h.addAlloc(structs.AllocClientStatusPending)
			if tt.isUnhelped {
				h.runner.capacity.replace(map[targetKey]string{key: "the pool is at its maximum"})
			}
			if tt.isDeploying {
				h.startDeployment(false)
			}

			// The group is seen unsettled now; the policy is evaluated
			// after the wait.
			h.runner.settle.observe(key, false, h.clock.Now())
			h.clock.Advance(tt.wait)
			must.NoError(t, h.evaluate())

			status := h.runner.status.get(h.policyID())
			must.NotNil(t, status)
			must.Eq(t, tt.expectedOutcome, status.Outcome)
			must.Eq(t, tt.expectedHold, status.HoldReason)
			if tt.expectedOutcome == OutcomeHeld {
				must.Len(t, 0, h.scaler.requests())
				must.Eq(t, "the pool is at its maximum", status.Message)
				return
			}
			reqs := h.scaler.requests()
			must.Len(t, 1, reqs)
			must.Eq(t, int64(6), *reqs[0].Count)
		})
	}
}
