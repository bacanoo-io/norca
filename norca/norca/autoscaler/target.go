// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"strconv"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

const (
	// metaKeyEngine is added to the meta of every scaling event written by
	// the built-in autoscaler, so its own decisions can be told apart from
	// manual or external scaling in the scaling_event history.
	metaKeyEngine = "nomad_autoscaler.engine"

	// metaKeySuspendedUntil is set on the warning event written when
	// oscillation protection suspends a task group. Its value is the end of
	// the suspension as Unix nanoseconds, formatted as a decimal string.
	metaKeySuspendedUntil = "nomad_autoscaler.suspended_until"
)

// IsOwnScaling reports whether the meta of a scaling request or event is that
// of an action of the built-in autoscaler.
func IsOwnScaling(meta map[string]interface{}) bool {
	return meta[metaKeyEngine] == structs.ScalingPolicyEngineBuiltin
}

// targetKey identifies a scaled task group.
type targetKey struct {
	namespace string
	job       string
	group     string

	// kind is empty for a task group; targetKindNodeGroup for a node group,
	// whose name is in job.
	kind string
}

func (k targetKey) String() string {
	if k.kind != "" {
		return fmt.Sprintf("%s/%s", k.kind, k.job)
	}
	return fmt.Sprintf("%s/%s/%s", k.namespace, k.job, k.group)
}

// targetKeyFromPolicy returns the task group targeted by a policy.
func targetKeyFromPolicy(p *structs.ScalingPolicy) (targetKey, bool) {
	if name := p.Target[structs.ScalingTargetNodeGroup]; name != "" {
		return targetKey{kind: targetKindNodeGroup, job: name}, true
	}
	k := targetKey{
		namespace: p.Target[structs.ScalingTargetNamespace],
		job:       p.Target[structs.ScalingTargetJob],
		group:     p.Target[structs.ScalingTargetGroup],
	}
	if k.namespace == "" {
		k.namespace = structs.DefaultNamespace
	}
	return k, k.job != "" && k.group != "" && p.Target[structs.ScalingTargetTask] == ""
}

// targetState is the view of a task group the evaluation works with. It
// replaces the status call of the external autoscaler's Nomad target plugin
// and is read directly from the state store.
type targetState struct {
	// status.Count is what runs now: the running allocations of the group
	// that are not canaries, or the replicas an object's status reports.
	status *sdk.TargetStatus

	// desired is what was asked for: the count of the group, or the spec
	// value of an object's scale subresource. It differs from status.Count
	// while allocations start, wait for capacity, migrate or are replaced.
	desired int64

	// isMigrating is set while allocations of the group are marked to
	// migrate (a node drain) and have not stopped yet: the running count is
	// about to drop.
	isMigrating bool

	// instances are the allocations of the group that count as instances
	// (no canaries), terminal ones included, for runningAverage. Nil for an
	// object target.
	instances []*structs.Allocation

	// job is the current version of the job and group its task group.
	job   *structs.Job
	group *structs.TaskGroup

	// deploymentActive is set while a deployment of the current job is
	// active, unpromotedCanary while it waits for canaries to be promoted,
	// and deploymentWaits while it waits for its turn in a multiregion
	// rollout.
	deploymentActive bool
	unpromotedCanary bool
	deploymentWaits  bool

	// events are the scaling events of the task group, newest first.
	events []*structs.ScalingEvent
}

// readTarget reads the target's current state. A nil result means the job
// does not exist (anymore).
func readTarget(store *state.StateStore, key targetKey) (*targetState, error) {
	job, err := store.JobByID(nil, key.namespace, key.job)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, nil
	}
	tg := job.LookupTaskGroup(key.group)
	if tg == nil {
		return nil, fmt.Errorf("task group %q not found", key.group)
	}

	allocs, err := store.AllocsByJob(nil, key.namespace, key.job, false)
	if err != nil {
		return nil, err
	}

	// Count the running allocations the same way Job.ScaleStatus does, which
	// is what the external autoscaler's Nomad target plugin uses as count.
	var running int64
	isMigrating := false
	instances := make([]*structs.Allocation, 0, len(allocs))
	for _, alloc := range allocs {
		if alloc.TaskGroup != key.group {
			continue
		}
		if alloc.DeploymentStatus != nil && alloc.DeploymentStatus.Canary {
			continue
		}
		instances = append(instances, alloc)
		if alloc.TerminalStatus() {
			continue
		}
		if alloc.ClientStatus == structs.AllocClientStatusRunning {
			running++
		}
		if alloc.DesiredTransition.ShouldMigrate() {
			isMigrating = true
		}
	}

	events, _, err := store.ScalingEventsByJob(nil, key.namespace, key.job)
	if err != nil {
		return nil, err
	}

	t := &targetState{
		status: &sdk.TargetStatus{
			Ready: !job.Stop,
			Count: running,
			Meta:  map[string]string{},
		},
		desired:     int64(tg.Count),
		isMigrating: isMigrating,
		instances:   instances,
		job:         job,
		group:       tg,
		events:      events[key.group],
	}

	// Same rule as Job.Scale: a deployment blocks scaling while it is active
	// and belongs to the current incarnation of the job.
	deployment, err := store.LatestDeploymentByJobID(nil, key.namespace, key.job)
	if err != nil {
		return nil, err
	}
	if deployment != nil && deployment.Active() && deployment.JobCreateIndex == job.CreateIndex {
		t.deploymentActive = true
		t.deploymentWaits = deployment.IsMultiregion && deployment.Status == structs.DeploymentStatusPending
		for _, ds := range deployment.TaskGroups {
			if ds != nil && ds.DesiredCanaries > 0 && !ds.Promoted {
				t.unpromotedCanary = true
			}
		}
	}

	return t, nil
}

// isSettled reports whether the target runs what was asked for: nothing is
// starting, waiting for capacity, being replaced or about to migrate. A
// target that is not settled is not scaled down (see runner.evaluate).
func (t *targetState) isSettled() bool {
	return t.status.Count >= t.desired && !t.isMigrating
}

// maxRunningAverageWindow bounds the range runningAverage looks back on:
// terminal allocations are removed from the state by the garbage collector,
// so a long range would miss the instances that ran at its beginning.
const maxRunningAverageWindow = 15 * time.Minute

// runningAverage returns the average number of running instances of the
// group over [from, to]: the population a value measured over that range
// belongs to. It is false when it cannot be told: an object target, an empty
// range or one longer than maxRunningAverageWindow.
func (t *targetState) runningAverage(from, to time.Time) (float64, bool) {
	window := to.Sub(from)
	if t.instances == nil || window <= 0 || window > maxRunningAverageWindow {
		return 0, false
	}
	var total time.Duration
	for _, alloc := range t.instances {
		start, end, ok := runningInterval(alloc, from, to)
		if !ok {
			continue
		}
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if end.After(start) {
			total += end.Sub(start)
		}
	}
	return float64(total) / float64(window), true
}

// runningInterval returns when an allocation was running, as far as its task
// states tell; it is false for one that never ran. Where the state does not
// say, the interval is the longer one: a value is rather laid on too many
// instances (a larger count results) than on too few.
//
//   - A running allocation runs from the latest start of its running tasks
//     to the end of the range. One whose tasks restarted within the range, or
//     whose task states have no start time, counts for the whole range.
//   - One that ran and stopped ran from the first start of its tasks to their
//     last finish; a lost allocation, whose tasks never reported a finish,
//     until the server last changed it.
func runningInterval(alloc *structs.Allocation, from, to time.Time) (start, end time.Time, ok bool) {
	if alloc.ClientStatus == structs.AllocClientStatusRunning {
		for _, ts := range alloc.TaskStates {
			if ts == nil || ts.State != structs.TaskStateRunning {
				continue
			}
			if ts.Restarts > 0 && ts.LastRestart.After(from) {
				return from, to, true
			}
			if ts.StartedAt.After(start) {
				start = ts.StartedAt
			}
		}
		if start.IsZero() {
			start = from
		}
		return start, to, true
	}

	for _, ts := range alloc.TaskStates {
		if ts == nil || ts.StartedAt.IsZero() {
			continue
		}
		if start.IsZero() || ts.StartedAt.Before(start) {
			start = ts.StartedAt
		}
		if ts.FinishedAt.After(end) {
			end = ts.FinishedAt
		}
	}
	if start.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	if end.IsZero() {
		end = time.Unix(0, alloc.ModifyTime)
	}
	return start, end, true
}

// cooldownUntil returns the end of the cooldown caused by the last scaling
// event that changed the count of the group, whoever made it (the built-in
// autoscaler, an operator or the external autoscaler). Because every scaling
// action is recorded in the scaling_event table, this also restores the
// cooldown after a leader change.
//
// An event that did not change the count (a scale request for the count the
// group already has) starts no cooldown: its direction is none, and it would
// hide the cooldown of the real change before it.
func (t *targetState) cooldownUntil(p *sdk.ScalingPolicy) time.Time {
	for _, e := range t.events {
		if e.Count == nil || e.Error || *e.Count == e.PreviousCount {
			continue
		}
		cd := p.Cooldown
		if *e.Count > e.PreviousCount {
			cd = p.CooldownOnScaleUp
		}
		return time.Unix(0, e.Time).Add(cd)
	}
	return time.Time{}
}

// suspendedUntil returns the end of the latest oscillation suspension
// recorded in the scaling events.
func (t *targetState) suspendedUntil() time.Time {
	for _, e := range t.events {
		if !e.Error || e.Meta == nil {
			continue
		}
		raw, ok := e.Meta[metaKeySuspendedUntil]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue
		}
		ns, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// recentDirections returns the directions of the count changes made by the
// built-in autoscaler since the given time, oldest first.
func (t *targetState) recentDirections(since time.Time) []sdk.ScaleDirection {
	var dirs []sdk.ScaleDirection
	for _, e := range t.events {
		if time.Unix(0, e.Time).Before(since) {
			break
		}
		if e.Count == nil || e.Error || e.Meta == nil || e.Meta[metaKeyEngine] != structs.ScalingPolicyEngineBuiltin {
			continue
		}
		switch {
		case *e.Count > e.PreviousCount:
			dirs = append(dirs, sdk.ScaleDirectionUp)
		case *e.Count < e.PreviousCount:
			dirs = append(dirs, sdk.ScaleDirectionDown)
		}
	}

	// Events are stored newest first.
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

// directionChanges counts how many times the direction changes along dirs.
func directionChanges(dirs []sdk.ScaleDirection) int {
	changes := 0
	for i := 1; i < len(dirs); i++ {
		if dirs[i] != dirs[i-1] {
			changes++
		}
	}
	return changes
}
