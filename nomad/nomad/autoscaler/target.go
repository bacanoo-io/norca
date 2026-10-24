// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"strconv"
	"time"

	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
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

// targetKey identifies a scaled task group.
type targetKey struct {
	namespace string
	job       string
	group     string
}

func (k targetKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.namespace, k.job, k.group)
}

// targetKeyFromPolicy returns the task group targeted by a policy.
func targetKeyFromPolicy(p *structs.ScalingPolicy) (targetKey, bool) {
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
	status *sdk.TargetStatus

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
	if job.LookupTaskGroup(key.group) == nil {
		return nil, fmt.Errorf("task group %q not found", key.group)
	}

	allocs, err := store.AllocsByJob(nil, key.namespace, key.job, false)
	if err != nil {
		return nil, err
	}

	// Count the running allocations the same way Job.ScaleStatus does, which
	// is what the external autoscaler's Nomad target plugin uses as count.
	var running int64
	for _, alloc := range allocs {
		if alloc.TaskGroup != key.group || alloc.TerminalStatus() {
			continue
		}
		if alloc.DeploymentStatus != nil && alloc.DeploymentStatus.Canary {
			continue
		}
		if alloc.ClientStatus == structs.AllocClientStatusRunning {
			running++
		}
	}

	events, _, err := store.ScalingEventsByJob(nil, key.namespace, key.job)
	if err != nil {
		return nil, err
	}

	return &targetState{
		status: &sdk.TargetStatus{
			Ready: !job.Stop,
			Count: running,
			Meta:  map[string]string{},
		},
		events: events[key.group],
	}, nil
}

// cooldownUntil returns the end of the cooldown caused by the last scaling
// event that changed the count of the group, whoever made it (the built-in
// autoscaler, an operator or the external autoscaler). Because every scaling
// action is recorded in the scaling_event table, this also restores the
// cooldown after a leader change.
func (t *targetState) cooldownUntil(p *sdk.ScalingPolicy) time.Time {
	for _, e := range t.events {
		if e.Count == nil || e.Error {
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
