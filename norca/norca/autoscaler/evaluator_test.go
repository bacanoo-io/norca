// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// simTarget is the load one instance is meant to carry in the simulations:
// the target of the check of targetValuePolicy.
const simTarget = 50

// simStep is the time between two evaluations of a simulation: the
// evaluation interval of targetValuePolicy.
const simStep = 10 * time.Second

// simAlloc is an allocation of the simulated group.
type simAlloc struct {
	id string
	// isRunning and isMigrating follow what the store holds; a stopped
	// allocation is removed from the simulation.
	isRunning   bool
	isMigrating bool
	// runAt is when a pending allocation starts to run.
	runAt time.Time
}

// simGroup plays the scheduler and the clients for the job of a harness: it
// keeps as many allocations as the count of the group asks for, runs them
// after a start delay, and lets a test drain, stop or lose them. The value
// of the harness APM is the load divided by the running allocations: what
// every instance shows when they share the load.
type simGroup struct {
	t *testing.T
	h *harness

	// load is the total load on the group, in the unit of simTarget.
	load float64

	// startDelay is how long an allocation is pending before it runs.
	startDelay time.Duration

	// capacity is the most allocations that can run (0: no limit); those
	// beyond it stay pending, as work that waits for a node.
	capacity int

	allocs []*simAlloc

	// seen is the number of scaling requests already checked by run, and
	// unsettledSince when run first saw the group not settled (zero while
	// it is settled).
	seen           int
	unsettledSince time.Time
}

// newSimGroup returns the simulation of a harness whose group runs count
// allocations and carries the load.
func newSimGroup(t *testing.T, h *harness, count int, load float64) *simGroup {
	t.Helper()
	g := &simGroup{t: t, h: h, load: load}

	existing, err := h.store.AllocsByJob(nil, h.job.Namespace, h.job.ID, false)
	must.NoError(t, err)
	for _, a := range existing {
		g.allocs = append(g.allocs, &simAlloc{id: a.ID, isRunning: true})
	}
	h.setCount(count)
	for len(g.allocs) < count {
		g.allocs = append(g.allocs, &simAlloc{id: h.addAlloc(structs.AllocClientStatusRunning), isRunning: true})
	}

	h.apm.valueFn = func() float64 {
		running := g.running()
		if running == 0 {
			return 0
		}
		return g.load / float64(running)
	}
	return g
}

func (g *simGroup) desired() int {
	g.t.Helper()
	job, err := g.h.store.JobByID(nil, g.h.job.Namespace, g.h.job.ID)
	must.NoError(g.t, err)
	return job.TaskGroups[0].Count
}

func (g *simGroup) running() int {
	n := 0
	for _, a := range g.allocs {
		if a.isRunning {
			n++
		}
	}
	return n
}

// stop ends the allocation at index i: complete, or lost with its node.
func (g *simGroup) stop(i int, clientStatus string) {
	g.t.Helper()
	a := g.allocs[i]
	g.h.updateAlloc(a.id, func(stored *structs.Allocation) {
		stored.DesiredStatus = structs.AllocDesiredStatusStop
		if clientStatus == structs.AllocClientStatusLost {
			stored.ClientStatus = clientStatus
		}
	})
	if clientStatus != structs.AllocClientStatusLost {
		g.h.setClientStatus(a.id, clientStatus)
	}
	g.allocs = append(g.allocs[:i], g.allocs[i+1:]...)
}

// reconcile does what the scheduler and the clients do between two
// evaluations: pending allocations whose start delay passed run (up to the
// capacity), missing allocations are placed as pending, surplus ones stop.
func (g *simGroup) reconcile() {
	g.t.Helper()
	now := g.h.clock.Now()

	for _, a := range g.allocs {
		if a.isRunning || a.runAt.After(now) {
			continue
		}
		if g.capacity > 0 && g.running() >= g.capacity {
			continue
		}
		g.h.setClientStatus(a.id, structs.AllocClientStatusRunning)
		a.isRunning = true
	}

	desired := g.desired()
	for len(g.allocs) < desired {
		id := g.h.addAlloc(structs.AllocClientStatusPending)
		g.allocs = append(g.allocs, &simAlloc{id: id, runAt: now.Add(g.startDelay)})
	}
	for len(g.allocs) > desired {
		g.stop(len(g.allocs)-1, structs.AllocClientStatusComplete)
	}
}

// drain marks n running allocations to migrate, as a node drain does.
func (g *simGroup) drain(n int) {
	g.t.Helper()
	for _, a := range g.allocs {
		if n == 0 {
			return
		}
		if !a.isRunning || a.isMigrating {
			continue
		}
		g.h.updateAlloc(a.id, func(stored *structs.Allocation) {
			stored.DesiredTransition.Migrate = pointer.Of(true)
		})
		a.isMigrating = true
		n--
	}
}

// stopMigrating stops the allocations marked by drain; reconcile then places
// their replacements.
func (g *simGroup) stopMigrating() {
	g.t.Helper()
	for i := len(g.allocs) - 1; i >= 0; i-- {
		if g.allocs[i].isMigrating {
			g.stop(i, structs.AllocClientStatusComplete)
		}
	}
}

// lose loses n running allocations with their node.
func (g *simGroup) lose(n int) {
	g.t.Helper()
	for i := len(g.allocs) - 1; i >= 0 && n > 0; i-- {
		if g.allocs[i].isRunning {
			g.stop(i, structs.AllocClientStatusLost)
			n--
		}
	}
}

// run lets the time pass: at every step the scheduler and the clients act,
// then the policy is evaluated. It checks the two rules of the decision
// model on every scaling request that is made:
//
//   - a request changes the count (nothing is asked again);
//   - the count is lowered only for a group that runs what was asked for and
//     has nothing migrating, unless it has been unsettled for longer than the
//     settle timeout.
func (g *simGroup) run(d time.Duration) {
	g.t.Helper()
	for elapsed := time.Duration(0); elapsed < d; elapsed += simStep {
		g.reconcile()

		desired, running := g.desired(), g.running()
		isSettled := running >= desired
		for _, a := range g.allocs {
			if a.isMigrating {
				isSettled = false
			}
		}
		now := g.h.clock.Now()
		switch {
		case isSettled:
			g.unsettledSince = time.Time{}
		case g.unsettledSince.IsZero():
			g.unsettledSince = now
		}
		var unsettledFor time.Duration
		if !isSettled {
			unsettledFor = now.Sub(g.unsettledSince)
		}

		must.NoError(g.t, g.h.evaluate())

		reqs := g.h.scaler.requests()
		for _, req := range reqs[g.seen:] {
			must.NotNil(g.t, req.Count)
			count := int(*req.Count)
			must.NotEq(g.t, desired, count, must.Sprintf("at %s: the count %d was asked again", now.Format(time.TimeOnly), desired))
			if count < desired && !isSettled {
				must.GreaterEq(g.t, g.h.cfg.SettleTimeout, unsettledFor, must.Sprintf(
					"at %s: scaled down from %d to %d while %d ran, unsettled for %s",
					now.Format(time.TimeOnly), desired, count, running, unsettledFor))
			}
		}
		g.seen = len(reqs)

		g.h.clock.Advance(simStep)
	}
}

// counts returns the counts of the scaling requests made so far.
func (g *simGroup) counts() []int {
	reqs := g.h.scaler.requests()
	counts := make([]int, 0, len(reqs))
	for _, req := range reqs {
		counts = append(counts, int(*req.Count))
	}
	return counts
}

// simHarness returns a harness whose policy scales between 1 and 20 with a
// short cooldown after a scale up, and the given settle timeout.
func simHarness(t *testing.T, settleTimeout time.Duration) *harness {
	t.Helper()
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin,
		targetValuePolicy(map[string]interface{}{"cooldown_on_scale_up": "20s"}),
		func(cfg *Config) { cfg.SettleTimeout = settleTimeout })

	job, err := h.store.JobByID(nil, h.job.Namespace, h.job.ID)
	must.NoError(t, err)
	job = job.Copy()
	job.TaskGroups[0].Scaling.Max = 20
	h.index++
	must.NoError(t, h.store.UpsertJob(structs.MsgTypeTestSetup, h.index, nil, job))
	h.sync()
	return h
}

// TestRunner_Evaluate_Simulation plays the cases in which the count of a
// group, what runs of it and what its values were measured on differ: the
// scale ups and scale downs must be those the load asks for, each made once.
func TestRunner_Evaluate_Simulation(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name          string
		settleTimeout time.Duration
		count         int
		load          float64
		play          func(g *simGroup)
		expected      []int
	}{
		{
			// The load of 2 instances grows to that of 10. The 8 new
			// allocations take 40s to run; the policy is evaluated every 10s
			// and its cooldown after a scale up is 20s.
			name:  "a load step is answered once while the allocations start",
			count: 2,
			load:  2 * simTarget,
			play: func(g *simGroup) {
				g.startDelay = 40 * time.Second
				g.run(time.Minute)
				g.load = 10 * simTarget
				g.run(3 * time.Minute)
			},
			expected: []int{10},
		},
		{
			name:  "more load while the allocations start is answered with the difference",
			count: 2,
			load:  2 * simTarget,
			play: func(g *simGroup) {
				g.startDelay = time.Minute
				g.load = 6 * simTarget
				g.run(30 * time.Second)
				g.load = 12 * simTarget
				g.run(4 * time.Minute)
			},
			expected: []int{6, 12},
		},
		{
			// Two of six allocations are drained under a steady load: first
			// marked, then stopped, their replacements start 30s later.
			name:  "a drain under a steady load changes nothing",
			count: 6,
			load:  6 * simTarget,
			play: func(g *simGroup) {
				g.startDelay = 30 * time.Second
				g.run(time.Minute)
				g.drain(2)
				g.run(20 * time.Second)
				g.stopMigrating()
				g.run(3 * time.Minute)
			},
			expected: []int{},
		},
		{
			name:  "a lost node under a steady load changes nothing",
			count: 6,
			load:  6 * simTarget,
			play: func(g *simGroup) {
				g.startDelay = 30 * time.Second
				g.run(time.Minute)
				g.lose(2)
				g.run(3 * time.Minute)
			},
			expected: []int{},
		},
		{
			// The load falls to that of 2 instances at the moment a node is
			// lost: the scale down waits for the replacements, then is made
			// once (run checks that it did not come before).
			name:  "a scale down waits until the replacements run",
			count: 6,
			load:  6 * simTarget,
			play: func(g *simGroup) {
				g.startDelay = time.Minute
				g.run(time.Minute)
				g.lose(2)
				g.load = 2 * simTarget
				g.run(5 * time.Minute)
			},
			expected: []int{2},
		},
		{
			// Six are asked for and only four can run; the load is that of
			// three. The two that wait hold the scale down until the settle
			// timeout, not longer.
			name:          "a count that never comes to run holds a scale down until the settle timeout",
			settleTimeout: 2 * time.Minute,
			count:         4,
			load:          3 * simTarget,
			play: func(g *simGroup) {
				g.capacity = 4
				g.h.setCount(6)
				g.run(110 * time.Second)
				must.Len(g.t, 0, g.counts(), must.Sprint("scaled down before the settle timeout"))
				g.run(2 * time.Minute)
			},
			expected: []int{3},
		},
		{
			name:  "a settled group is scaled down at once",
			count: 6,
			load:  3 * simTarget,
			play: func(g *simGroup) {
				g.run(30 * time.Second)
			},
			expected: []int{3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			settleTimeout := tt.settleTimeout
			if settleTimeout == 0 {
				settleTimeout = DefaultSettleTimeout
			}
			g := newSimGroup(t, simHarness(t, settleTimeout), tt.count, tt.load)
			tt.play(g)

			must.Eq(t, tt.expected, g.counts())
			// In the end the group is asked to run what the load needs.
			must.Eq(t, int(g.load/simTarget), g.desired())
		})
	}
}

// A check whose values come from a plugin is taken per instance of what runs
// now: with two of four running at twice the target, the four that were
// asked for are what the load needs.
func TestRunner_Evaluate_NoRequestForWhatIsOnItsWay(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)

	// 2 running at 100 against a target of 50, and 4 already asked for.
	h.setCount(4)
	h.addAlloc(structs.AllocClientStatusPending)
	h.addAlloc(structs.AllocClientStatusPending)

	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())
}

// A result below the count that was asked for is a scale down, whatever runs:
// it must not be made as a "scale up" from the running count.
func TestRunner_Evaluate_UpNeverLowersTheCount(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)

	// 2 running at 75: the load needs 3. 6 were asked for, 4 are starting.
	h.apm.value = 75
	h.setCount(6)
	for range 4 {
		h.addAlloc(structs.AllocClientStatusPending)
	}

	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests(), must.Sprint("the count was lowered while allocations were starting"))
}

func TestRunner_LoadCheckRunners_Scope(t *testing.T) {
	ci.Parallel(t)

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.withUsage(time.Minute)
	host := h.runner.host
	host.apms["fake"] = h.apm

	check := func(source, query, strategy string) *sdk.ScalingPolicyCheck {
		return &sdk.ScalingPolicyCheck{
			Name:     "check",
			Source:   source,
			Query:    query,
			Strategy: &sdk.ScalingPolicyStrategy{Name: strategy},
		}
	}
	groupTarget := &sdk.ScalingPolicyTarget{Config: map[string]string{
		sdk.TargetConfigKeyNamespace: "default",
		sdk.TargetConfigKeyJob:       "shop",
		sdk.TargetConfigKeyTaskGroup: "web",
	}}

	tests := []struct {
		name     string
		policyID string
		check    *sdk.ScalingPolicyCheck
		expected valueScope
	}{
		{
			name:     "a plugin's value is per instance of what runs",
			check:    check("fake", "cpu", plugins.InternalStrategyTargetValue),
			expected: valueScopeRunning,
		},
		{
			name:     "the usage of the own group is per instance of the window",
			check:    check(plugins.InternalAPMNomad, MetricCPU, plugins.InternalStrategyTargetValue),
			expected: valueScopeWindow,
		},
		{
			name:     "a sum of the usage is used as it is",
			check:    check(plugins.InternalAPMNomad, "taskgroup_sum_cpu-allocated", plugins.InternalStrategyTargetValue),
			expected: valueScopeUnknown,
		},
		{
			name:     "the capacity of the cluster says nothing of a task group",
			check:    check(plugins.InternalAPMNorca, "cluster.cpu_allocated", plugins.InternalStrategyThreshold),
			expected: valueScopeUnknown,
		},
		{
			name:     "the capacity of the cluster is the use of a node group's nodes",
			policyID: nodeGroupPolicyPrefix + "workers-a",
			check:    check(plugins.InternalAPMNorca, "cluster.cpu_allocated", plugins.InternalStrategyTargetValue),
			expected: valueScopeRunning,
		},
		{
			name:     "a value handed on as the count is used as it is",
			check:    check("fake", "wanted", plugins.InternalStrategyPassThrough),
			expected: valueScopeUnknown,
		},
		{
			name:     "a fixed value has no measured value",
			check:    check("", "", plugins.InternalStrategyFixedValue),
			expected: valueScopeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := &sdk.ScalingPolicy{
				ID:     tt.policyID,
				Checks: []*sdk.ScalingPolicyCheck{tt.check},
				Target: groupTarget,
			}
			runners, err := h.runner.loadCheckRunners(policy)
			must.NoError(t, err)
			must.Len(t, 1, runners)
			runner, ok := runners[0].(*checkRunner)
			must.True(t, ok)
			must.Eq(t, tt.expected, runner.scope)
		})
	}
}

// The usage the clients report is an average over the query window, so it is
// laid on the average number of instances that ran in that window. Two of six
// allocations stopped at half of the window (a drain): the four that stay
// carry 75 each instead of 50, the reports average to 60 over 5 instances,
// and that is the 50 the six that were asked for will show: nothing to do.
//
// Taken on the 4 that run at the moment of the evaluation, the same 60 would
// ask for 5, a "scale up" from 4 that lowers the count from 6 during the
// drain.
func TestRunner_Evaluate_DrainWithinTheWindow(t *testing.T) {
	ci.Parallel(t)

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, usagePolicy(MetricCPU, 50), nil)
	collector := h.withUsage(time.Minute)
	group := h.job.TaskGroups[0].Name
	start := h.clock.Now()

	// Six run; the harness made two of them.
	h.setCount(6)
	var drained []string
	for i := range 4 {
		id := h.addAlloc(structs.AllocClientStatusRunning)
		if i < 2 {
			drained = append(drained, id)
		}
	}

	// First half of the window: six at 50%.
	h.clock.Advance(30 * time.Second)
	collector.record(h.clock.Now(), usageReport(h.job.ID, group, 60, 50, 100))

	// Two stop; their replacements are placed and have not started.
	for _, id := range drained {
		h.finishAlloc(id, start.Add(-time.Hour), h.clock.Now())
		h.addAlloc(structs.AllocClientStatusPending)
	}

	// Second half: four at 75%.
	h.clock.Advance(30 * time.Second)
	collector.record(h.clock.Now(), usageReport(h.job.ID, group, 40, 75, 100))

	must.NoError(t, h.evaluate())
	must.Len(t, 0, h.scaler.requests())
}
