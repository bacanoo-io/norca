// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// EventPublisher publishes the events of the Scaling topic.
type EventPublisher func(eventType string, ev *structs.ScalingDecisionEvent)

// capAtQuota caps a scale up at the quota ceiling of the job's namespace:
// the desired count of the group plus the number of allocations of the
// group's size that still fit into the quota. It returns false if the group
// can't grow at all. Both cases publish a ScalingQuotaCapped event.
func (r *runner) capAtQuota(store *state.StateStore, c *compiledPolicy, t *targetState,
	action *sdk.ScalingAction, check string, now time.Time) (bool, error) {

	remaining, err := store.QuotaRemainingForNamespace(nil, c.key.namespace)
	if err != nil {
		return false, err
	}
	if remaining == nil {
		return true, nil
	}
	fit := remaining.MaxAllocs(structs.TaskGroupQuotaResources(t.group, 0))
	if fit < 0 {
		return true, nil
	}

	base := int64(t.group.Count)
	if c.object != nil {
		base = t.status.Count
	}
	ceiling := base + fit
	if action.Count <= ceiling {
		return true, nil
	}

	metrics.IncrCounter([]string{"nomad", "autoscaler", "scale", "quota_capped"}, 1)
	r.logger.Info("scale up capped by the namespace quota", "policy_id", c.policy.ID,
		"target", c.key.String(), "quota", remaining.Quota, "desired", action.Count, "ceiling", ceiling)

	r.exec.publish(structs.TypeScalingQuotaCapped, &structs.ScalingDecisionEvent{
		Namespace:     c.key.namespace,
		JobID:         c.key.job,
		Group:         c.key.group,
		PolicyID:      c.policy.ID,
		PreviousCount: t.status.Count,
		Count:         ceiling,
		DesiredCount:  action.Count,
		Direction:     sdk.ScaleDirection(sdk.ScaleDirectionUp).String(),
		Check:         check,
		Reason:        action.Reason,
		Quota:         remaining.Quota,
		Time:          now.UnixNano(),
	})

	if ceiling <= t.status.Count || ceiling < c.policy.Min {
		return false, nil
	}
	action.Reason = fmt.Sprintf("%s (capped from %d to %d by quota %q)",
		action.Reason, action.Count, ceiling, remaining.Quota)
	action.Count = ceiling
	return true, nil
}

// publish publishes an event of the Scaling topic, if a publisher is set.
func (e *executor) publish(eventType string, ev *structs.ScalingDecisionEvent) {
	if e.events != nil {
		e.events(eventType, ev)
	}
}
