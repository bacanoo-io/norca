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
	hclog "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics"
)

// executeObject performs a scaling action on a custom object target, after
// the oscillation protection and the rate limit of execute. The scale
// subresource records the change in the object's event log, which the
// cooldown is derived from.
func (e *executor) executeObject(c *compiledPolicy, t *targetState, action sdk.ScalingAction, check string,
	now time.Time, dryRun bool, logger hclog.Logger) error {

	o := c.object
	meta := map[string]string{
		metaKeyEngine:     structs.ScalingPolicyEngineBuiltin,
		"nomad_policy_id": c.policy.ID,
	}
	namespace := structs.WriteRequest{Namespace: o.namespace}

	if dryRun {
		count := action.Count
		if n, ok := action.Meta["nomad_autoscaler.dry_run.count"].(int64); ok {
			count = n
		}
		meta["dry_run_count"] = strconv.FormatInt(count, 10)
		err := e.objects.RecordObjectEvents(&structs.CustomObjectEventsRequest{
			Group: o.group,
			Kind:  o.kind,
			Name:  o.name,
			Events: []*structs.ObjectEvent{{
				Type:    structs.ObjectEventTypeNormal,
				Reason:  objectReasonDryRun,
				Message: fmt.Sprintf("autoscaler (dry run): would scale to %d: %s", count, action.Reason),
				Meta:    meta,
			}},
			WriteRequest: namespace,
		})
		if err != nil {
			return fmt.Errorf("failed to record the dry run of %s: %w", o, err)
		}
		return nil
	}

	// (Norca) Two deciders write the scale of an object as an absolute value
	// from their own reading: the evaluation of a policy and the blocked work
	// loop of a node group. The scale is written only if it is still the
	// one the decision was made on, and with the index it was checked at;
	// otherwise the decision is made again on the next look (review finding
	// ASC-7).
	cas, err := e.checkObjectScale(c, t)
	if err != nil {
		return err
	}

	start := time.Now()
	_, err = e.objects.ScaleObject(&structs.CustomObjectScaleRequest{
		Group:        o.group,
		Kind:         o.kind,
		Name:         o.name,
		Replicas:     action.Count,
		Message:      action.Reason,
		Meta:         meta,
		CAS:          cas,
		WriteRequest: namespace,
	})
	metrics.MeasureSince([]string{"autoscaler", "scale", "invoke"}, start)
	if err != nil {
		metrics.IncrCounter([]string{"autoscaler", "scale", "error"}, 1)
		return fmt.Errorf("failed to scale %s: %w", o, err)
	}
	metrics.IncrCounter([]string{"autoscaler", "scale", "success"}, 1)

	cd := c.policy.Cooldown
	if action.Direction == sdk.ScaleDirectionUp {
		cd = c.policy.CooldownOnScaleUp
	}
	e.l.Lock()
	e.cooldowns[c.key] = now.Add(cd)
	e.l.Unlock()

	logger.Info("scaled custom object", "from", t.desired, "to", action.Count,
		"reason", action.Reason, "cooldown", cd)

	e.publish(structs.TypeScalingDecision, &structs.ScalingDecisionEvent{
		Namespace:     c.key.namespace,
		JobID:         c.key.job,
		Group:         c.key.group,
		PolicyID:      c.policy.ID,
		PreviousCount: t.desired,
		Count:         action.Count,
		Direction:     action.Direction.String(),
		Check:         check,
		Reason:        fmt.Sprintf("%s (custom object %s)", action.Reason, o),
		Time:          now.UnixNano(),
	})
	return nil
}

// suspendObject records the oscillation suspension of a custom object
// target as a warning in the object's event log, the durable record
// restored after a leader change.
func (e *executor) suspendObject(c *compiledPolicy, until time.Time, logger hclog.Logger) {
	o := c.object
	err := e.objects.RecordObjectEvents(&structs.CustomObjectEventsRequest{
		Group: o.group,
		Kind:  o.kind,
		Name:  o.name,
		Events: []*structs.ObjectEvent{{
			Type:   structs.ObjectEventTypeWarning,
			Reason: objectReasonSuspended,
			Message: fmt.Sprintf("autoscaler: scaling direction changed %d times within %s; policy suspended for %s",
				e.cfg.OscillationThreshold, e.cfg.OscillationWindow, e.cfg.OscillationSuspend),
			Meta: map[string]string{
				metaKeyEngine:         structs.ScalingPolicyEngineBuiltin,
				metaKeySuspendedUntil: strconv.FormatInt(until.UnixNano(), 10),
				"nomad_policy_id":     c.policy.ID,
			},
		}},
		WriteRequest: structs.WriteRequest{Namespace: o.namespace},
	})
	if err != nil {
		logger.Error("failed to record the oscillation warning event", "error", err)
	}
}

// newObjectCheckedExecutor gives the executor the state to check the scale of
// an object against before it writes it.
func newObjectCheckedExecutor(e *executor, st func() *state.StateStore) *executor {
	e.state = st
	return e
}

// checkObjectScale reads the object again and returns its index for the
// check-and-set of the scale, or an error when its scale is not the one the
// decision was made on any more.
func (e *executor) checkObjectScale(c *compiledPolicy, t *targetState) (*uint64, error) {
	if e.state == nil || t == nil {
		return nil, nil
	}
	cur, err := readObjectTarget(e.state(), c.key, c.object)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s before scaling it: %w", c.object, err)
	}
	if cur == nil {
		return nil, fmt.Errorf("%s is gone", c.object)
	}
	if cur.desired != t.desired {
		return nil, fmt.Errorf("the scale of %s changed from %d to %d since the decision; it is decided again",
			c.object, t.desired, cur.desired)
	}
	index := cur.modifyIndex
	return &index, nil
}
