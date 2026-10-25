// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/structs"
	"golang.org/x/time/rate"
)

// JobScaler executes a scaling action. On the server it is the internal
// Job.Scale RPC, made with the autoscaler's system identity which only holds
// the scale-job capability. The implementation fills in the region and the
// authentication token.
type JobScaler interface {
	Scale(args *structs.JobScaleRequest) (*structs.JobRegisterResponse, error)
}

// errActiveDeployment is the error message of Job.Scale when a deployment of
// the job is running.
const errActiveDeployment = "job scaling blocked due to active deployment"

// executor turns scaling decisions into Job.Scale calls. It applies the
// cluster-wide rate limit and the per task group oscillation protection, and
// tracks the cooldowns that are not visible in the scaling_event table.
//
// It replaces the external autoscaler's Nomad target plugin
// (plugins/builtin/target/nomad), rewritten as an internal call.
type executor struct {
	logger  hclog.Logger
	cfg     *Config
	scaler  JobScaler
	objects ObjectScaler
	events  EventPublisher
	limiter *rate.Limiter

	// l protects the maps below.
	l sync.Mutex

	// cooldowns are set after a successful action. Most cooldowns can also be
	// derived from the scaling events, but not those of dry-run actions or of
	// an event that failed to be written.
	cooldowns map[targetKey]time.Time

	// suspended tracks the oscillation suspensions decided by this leader.
	suspended map[targetKey]time.Time
}

func newExecutor(logger hclog.Logger, cfg *Config, scaler JobScaler, objects ObjectScaler, events EventPublisher) *executor {
	return &executor{
		logger:  logger,
		cfg:     cfg,
		scaler:  scaler,
		objects: objects,
		events:  events,
		// The bucket starts full. It is always used with AllowN and the
		// injected clock, which keeps it deterministic in tests.
		limiter:   rate.NewLimiter(rate.Limit(cfg.RateLimit), cfg.RateLimitBurst),
		cooldowns: map[targetKey]time.Time{},
		suspended: map[targetKey]time.Time{},
	}
}

// blockedUntil returns the time until which the target must not be scaled,
// and why.
func (e *executor) blockedUntil(key targetKey, p *sdk.ScalingPolicy, t *targetState) (time.Time, string) {
	e.l.Lock()
	until, reason := e.cooldowns[key], "cooldown"
	suspended := e.suspended[key]
	e.l.Unlock()

	if cd := t.cooldownUntil(p); cd.After(until) {
		until = cd
	}
	if s := t.suspendedUntil(); s.After(suspended) {
		suspended = s
	}
	if suspended.After(until) {
		until, reason = suspended, "oscillation suspension"
	}
	return until, reason
}

// forget drops the state of a target that is no longer scaled.
func (e *executor) forget(key targetKey) {
	e.l.Lock()
	defer e.l.Unlock()
	delete(e.cooldowns, key)
	delete(e.suspended, key)
}

// execute performs the scaling action on the target. It returns an error if
// the action failed; skipped actions (rate limit, oscillation, active
// deployment) are not errors.
func (e *executor) execute(c *compiledPolicy, t *targetState, action sdk.ScalingAction, check string, now time.Time) error {
	key := c.key
	logger := e.logger.With("policy_id", c.policy.ID, "target", key.String())

	// If the policy is configured with dry-run:true then we set the action
	// count to nil so its no-nop. This allows us to still register a scaling
	// event, but not alter the job's state.
	if val, ok := c.policy.Target.Config["dry-run"]; ok && val == "true" {
		logger.Info("scaling dry-run is enabled, using no-op task group count")
		action.SetDryRun()
	}

	dryRun := action.Count == sdk.StrategyActionMetaValueDryRunCount

	// Oscillation protection: if this action would make the group change
	// direction OscillationThreshold times within OscillationWindow, the
	// group is suspended instead.
	if !dryRun && (action.Direction == sdk.ScaleDirectionUp || action.Direction == sdk.ScaleDirectionDown) {
		dirs := append(t.recentDirections(now.Add(-e.cfg.OscillationWindow)), action.Direction)
		if directionChanges(dirs) >= e.cfg.OscillationThreshold {
			e.suspend(c, now, logger)
			return nil
		}
	}

	// Cluster-wide token bucket on scaling actions. Actions over the limit
	// are dropped; the policy is evaluated again at its next interval, with
	// fresh metrics.
	if !e.limiter.AllowN(now, 1) {
		metrics.IncrCounter([]string{"nomad", "autoscaler", "scale", "rate_limited"}, 1)
		logger.Debug("scaling action dropped by the rate limit", "count", action.Count)
		return nil
	}

	if c.object != nil {
		return e.executeObject(c, t, action, check, now, dryRun, logger)
	}

	meta := make(map[string]interface{}, len(action.Meta)+1)
	for k, v := range action.Meta {
		meta[k] = v
	}
	meta[metaKeyEngine] = structs.ScalingPolicyEngineBuiltin

	req := &structs.JobScaleRequest{
		JobID: key.job,
		Target: map[string]string{
			structs.ScalingTargetNamespace: key.namespace,
			structs.ScalingTargetJob:       key.job,
			structs.ScalingTargetGroup:     key.group,
		},
		Message: action.Reason,
		Error:   action.Error,
		Meta:    meta,
		WriteRequest: structs.WriteRequest{
			Namespace: key.namespace,
		},
	}
	if !dryRun {
		count := action.Count
		req.Count = &count
	}

	start := time.Now()
	_, err := e.scaler.Scale(req)
	metrics.MeasureSince([]string{"nomad", "autoscaler", "scale", "invoke"}, start)
	if err != nil {
		// Active deployments errors are fairly common and usually not
		// impactful to the target's eventual end state, so treat them as a
		// no-op without cooldown.
		if strings.Contains(err.Error(), errActiveDeployment) {
			metrics.IncrCounter([]string{"nomad", "autoscaler", "scale", "noop"}, 1)
			logger.Debug("skipping scaling due to active deployment")
			return nil
		}
		metrics.IncrCounter([]string{"nomad", "autoscaler", "scale", "error"}, 1)
		return fmt.Errorf("failed to scale group %s: %w", key, err)
	}
	metrics.IncrCounter([]string{"nomad", "autoscaler", "scale", "success"}, 1)

	cd := c.policy.Cooldown
	if action.Direction == sdk.ScaleDirectionUp {
		cd = c.policy.CooldownOnScaleUp
	}
	e.l.Lock()
	e.cooldowns[key] = now.Add(cd)
	e.l.Unlock()

	logger.Info("scaled task group", "from", t.status.Count, "to", action.Count,
		"reason", action.Reason, "cooldown", cd)

	ev := &structs.ScalingDecisionEvent{
		Namespace:     key.namespace,
		JobID:         key.job,
		Group:         key.group,
		PolicyID:      c.policy.ID,
		PreviousCount: t.status.Count,
		Count:         action.Count,
		Direction:     action.Direction.String(),
		Check:         check,
		Reason:        action.Reason,
		DryRun:        dryRun,
		Time:          now.UnixNano(),
	}
	if dryRun {
		if n, ok := action.Meta["nomad_autoscaler.dry_run.count"].(int64); ok {
			ev.Count = n
		}
		ev.Direction = sdk.ScaleDirection(sdk.ScaleDirectionNone).String()
	}
	e.publish(structs.TypeScalingDecision, ev)
	return nil
}

// suspend suspends a task group for OscillationSuspend and records it as an
// error scaling event, which is both the warning for operators and the
// durable record restored after a leader change.
func (e *executor) suspend(c *compiledPolicy, now time.Time, logger hclog.Logger) {
	until := now.Add(e.cfg.OscillationSuspend)
	e.l.Lock()
	e.suspended[c.key] = until
	e.l.Unlock()

	metrics.IncrCounter([]string{"nomad", "autoscaler", "oscillation", "suspended"}, 1)
	logger.Warn("task group scaling oscillates, suspending the policy",
		"direction_changes", e.cfg.OscillationThreshold, "window", e.cfg.OscillationWindow,
		"suspended_until", until)

	if c.object != nil {
		e.suspendObject(c, until, logger)
		return
	}

	_, err := e.scaler.Scale(&structs.JobScaleRequest{
		JobID: c.key.job,
		Target: map[string]string{
			structs.ScalingTargetNamespace: c.key.namespace,
			structs.ScalingTargetJob:       c.key.job,
			structs.ScalingTargetGroup:     c.key.group,
		},
		Message: fmt.Sprintf("autoscaler: scaling direction changed %d times within %s; policy suspended for %s",
			e.cfg.OscillationThreshold, e.cfg.OscillationWindow, e.cfg.OscillationSuspend),
		Error: true,
		Meta: map[string]interface{}{
			metaKeyEngine:         structs.ScalingPolicyEngineBuiltin,
			metaKeySuspendedUntil: strconv.FormatInt(until.UnixNano(), 10),
			"nomad_policy_id":     c.policy.ID,
		},
		WriteRequest: structs.WriteRequest{
			Namespace: c.key.namespace,
		},
	})
	if err != nil {
		logger.Error("failed to record the oscillation warning event", "error", err)
	}
}
