// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// instanceAlloc returns an allocation of the group "web" in the given client
// status whose single task started and finished at the given offsets from
// base (a zero offset is "not set").
func instanceAlloc(base time.Time, clientStatus string, started, finished time.Duration) *structs.Allocation {
	state := &structs.TaskState{State: structs.TaskStateRunning}
	if clientStatus != structs.AllocClientStatusRunning {
		state.State = structs.TaskStateDead
	}
	if started != 0 {
		state.StartedAt = base.Add(started)
	}
	if finished != 0 {
		state.FinishedAt = base.Add(finished)
	}
	return &structs.Allocation{
		TaskGroup:    "web",
		ClientStatus: clientStatus,
		TaskStates:   map[string]*structs.TaskState{"web": state},
	}
}

func TestReadTarget(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	key := targetKey{namespace: h.job.Namespace, job: h.job.ID, group: "web"}

	// The harness job asks for 2 and runs 2.
	ts, err := readTarget(h.store, key)
	must.NoError(t, err)
	must.Eq(t, int64(2), ts.desired)
	must.Eq(t, int64(2), ts.status.Count)
	must.False(t, ts.isMigrating)
	must.True(t, ts.isSettled())

	// A third allocation is asked for and has not started.
	h.setCount(3)
	pending := h.addAlloc(structs.AllocClientStatusPending)
	ts, err = readTarget(h.store, key)
	must.NoError(t, err)
	must.Eq(t, int64(3), ts.desired)
	must.Eq(t, int64(2), ts.status.Count)
	must.False(t, ts.isSettled())

	// It runs; then one of the three is marked to migrate by a drain.
	h.setClientStatus(pending, structs.AllocClientStatusRunning)
	ts, err = readTarget(h.store, key)
	must.NoError(t, err)
	must.True(t, ts.isSettled())

	h.updateAlloc(pending, func(a *structs.Allocation) { a.DesiredTransition.Migrate = pointer.Of(true) })
	ts, err = readTarget(h.store, key)
	must.NoError(t, err)
	must.Eq(t, int64(3), ts.status.Count)
	must.True(t, ts.isMigrating)
	must.False(t, ts.isSettled())

	// A canary is not an instance.
	canary := h.addAlloc(structs.AllocClientStatusRunning)
	h.updateAlloc(canary, func(a *structs.Allocation) {
		a.DeploymentStatus = &structs.AllocDeploymentStatus{Canary: true}
	})
	ts, err = readTarget(h.store, key)
	must.NoError(t, err)
	must.Eq(t, int64(3), ts.status.Count)
	must.Len(t, 3, ts.instances)
}

func TestTargetState_RunningAverage(t *testing.T) {
	ci.Parallel(t)

	base := time.Unix(1_700_000_000, 0)
	from, to := base, base.Add(time.Minute)
	const (
		running  = structs.AllocClientStatusRunning
		complete = structs.AllocClientStatusComplete
		lost     = structs.AllocClientStatusLost
		pending  = structs.AllocClientStatusPending
	)
	// before is a start time before the range.
	const before = -10 * time.Minute

	restarted := instanceAlloc(base, running, 50*time.Second, 0)
	restarted.TaskStates["web"].Restarts = 1
	restarted.TaskStates["web"].LastRestart = base.Add(45 * time.Second)

	lostAlloc := instanceAlloc(base, lost, before, 0)
	lostAlloc.ModifyTime = base.Add(15 * time.Second).UnixNano()

	noStates := &structs.Allocation{TaskGroup: "web", ClientStatus: running}

	tests := []struct {
		name       string
		instances  []*structs.Allocation
		from, to   time.Time
		expected   float64
		expectedOK bool
	}{
		{
			name:       "steady: every instance ran the whole range",
			instances:  []*structs.Allocation{instanceAlloc(base, running, before, 0), instanceAlloc(base, running, before, 0)},
			from:       from,
			to:         to,
			expected:   2,
			expectedOK: true,
		},
		{
			name: "two of six stopped at half of the range",
			instances: []*structs.Allocation{
				instanceAlloc(base, running, before, 0), instanceAlloc(base, running, before, 0),
				instanceAlloc(base, running, before, 0), instanceAlloc(base, running, before, 0),
				instanceAlloc(base, complete, before, 30*time.Second), instanceAlloc(base, complete, before, 30*time.Second),
			},
			from:       from,
			to:         to,
			expected:   5,
			expectedOK: true,
		},
		{
			name: "one started at three quarters of the range",
			instances: []*structs.Allocation{
				instanceAlloc(base, running, before, 0),
				instanceAlloc(base, running, 45*time.Second, 0),
			},
			from:       from,
			to:         to,
			expected:   1.25,
			expectedOK: true,
		},
		{
			name:       "a pending instance never ran",
			instances:  []*structs.Allocation{instanceAlloc(base, running, before, 0), instanceAlloc(base, pending, 0, 0)},
			from:       from,
			to:         to,
			expected:   1,
			expectedOK: true,
		},
		{
			name:       "a lost instance ran until the server changed it",
			instances:  []*structs.Allocation{lostAlloc},
			from:       from,
			to:         to,
			expected:   0.25,
			expectedOK: true,
		},
		{
			name:       "a restart within the range counts for the whole range",
			instances:  []*structs.Allocation{restarted},
			from:       from,
			to:         to,
			expected:   1,
			expectedOK: true,
		},
		{
			name:       "a running instance without task states counts for the whole range",
			instances:  []*structs.Allocation{noStates},
			from:       from,
			to:         to,
			expected:   1,
			expectedOK: true,
		},
		{
			name:       "stopped before the range",
			instances:  []*structs.Allocation{instanceAlloc(base, complete, before, -time.Minute)},
			from:       from,
			to:         to,
			expected:   0,
			expectedOK: true,
		},
		{
			name:       "an empty group",
			instances:  []*structs.Allocation{},
			from:       from,
			to:         to,
			expected:   0,
			expectedOK: true,
		},
		{
			name:       "an object target has no instances",
			instances:  nil,
			from:       from,
			to:         to,
			expectedOK: false,
		},
		{
			name:       "an instant range",
			instances:  []*structs.Allocation{noStates},
			from:       to,
			to:         to,
			expectedOK: false,
		},
		{
			name:       "a range beyond what the state keeps",
			instances:  []*structs.Allocation{noStates},
			from:       to.Add(-maxRunningAverageWindow - time.Second),
			to:         to,
			expectedOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := &targetState{instances: tt.instances}
			got, ok := ts.runningAverage(tt.from, tt.to)
			must.Eq(t, tt.expectedOK, ok)
			must.InDelta(t, tt.expected, got, 1e-9)
		})
	}
}

func TestTargetState_CooldownUntil(t *testing.T) {
	ci.Parallel(t)

	const (
		cooldown   = 5 * time.Minute
		cooldownUp = 20 * time.Second
	)
	policy := &sdk.ScalingPolicy{Cooldown: cooldown, CooldownOnScaleUp: cooldownUp}

	base := time.Unix(1_700_000_000, 0)
	at := func(d time.Duration) int64 { return base.Add(d).UnixNano() }
	scaled := func(d time.Duration, prev, count int64) *structs.ScalingEvent {
		return &structs.ScalingEvent{Time: at(d), PreviousCount: prev, Count: pointer.Of(count)}
	}

	// Events are stored newest first.
	tests := []struct {
		name     string
		events   []*structs.ScalingEvent
		expected time.Time
	}{
		{
			name:     "no events",
			events:   nil,
			expected: time.Time{},
		},
		{
			name:     "scale up",
			events:   []*structs.ScalingEvent{scaled(0, 4, 6)},
			expected: base.Add(cooldownUp),
		},
		{
			name:     "scale down",
			events:   []*structs.ScalingEvent{scaled(0, 6, 4)},
			expected: base.Add(cooldown),
		},
		{
			name: "unchanged count after a scale up keeps the scale up cooldown",
			events: []*structs.ScalingEvent{
				scaled(30*time.Second, 6, 6),
				scaled(0, 4, 6),
			},
			expected: base.Add(cooldownUp),
		},
		{
			name:     "only an unchanged count",
			events:   []*structs.ScalingEvent{scaled(0, 6, 6)},
			expected: time.Time{},
		},
		{
			name: "informational and failed events are skipped",
			events: []*structs.ScalingEvent{
				{Time: at(2 * time.Minute), PreviousCount: 6},
				{Time: at(time.Minute), PreviousCount: 6, Count: pointer.Of(int64(8)), Error: true},
				scaled(0, 6, 4),
			},
			expected: base.Add(cooldown),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := &targetState{events: tt.events}
			must.Eq(t, tt.expected, ts.cooldownUntil(policy))
		})
	}
}
