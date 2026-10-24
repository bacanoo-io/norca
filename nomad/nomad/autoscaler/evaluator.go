// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Rewritten from Nomad Autoscaler v0.6.0 policy/handler.go. The external
// autoscaler runs one goroutine per policy with its own ticker; the built-in
// autoscaler schedules all policies from a single loop and evaluates them on a
// bounded worker pool. calculateNewCount, scalingNeeded,
// pickWinnerActionFromGroups, checkResult and loadCheckRunners are ported
// from handler.go with small changes.

package autoscaler

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	metrics "github.com/armon/go-metrics"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/structs"
)

var (
	// errTargetNotFound is used by a check handler to indicate the policy target
	// does not exist or is unavailable.
	errTargetNotFound = errors.New("target not found")
	errNoMetrics      = errors.New("no metrics available")
)

type checker interface {
	runCheckAndCapCount(ctx context.Context, currentCount int64, cache *queryMetricsCache) (sdk.ScalingAction, error)
	group() string
	name() string
}

// compiledPolicy is an immutable, ready to evaluate form of a builtin
// scaling policy.
type compiledPolicy struct {
	policy       *sdk.ScalingPolicy
	modifyIndex  uint64
	key          targetKey
	checkRunners []checker

	// object is the custom object the policy scales instead of its task
	// group, if any.
	object   *objectTarget
	schedule *compiledSchedule

	// scaleDuringDeployment is the policy's scale_during_deployment.
	scaleDuringDeployment string
}

// policyHandle is the scheduling state of one policy. It is owned by the
// runner loop.
type policyHandle struct {
	// compiled is nil if the policy has never been valid.
	compiled    *compiledPolicy
	modifyIndex uint64

	nextEval time.Time
	queued   bool
	inFlight bool
}

// evalResult is sent by the workers to the runner loop when an evaluation
// finishes.
type evalResult struct {
	id  string
	err error
}

// runner is one run of the built-in autoscaler on the leader. It is created
// when the autoscaler starts and discarded when it stops.
type runner struct {
	logger hclog.Logger
	cfg    *Config
	deps   *Deps
	now    func() time.Time
	host   *pluginHost
	proc   *processor
	exec   *executor

	// usage is the collector of the client pushed task group usage that
	// serves the checks of the nomad-apm source. Nil in tests that use the
	// plugin host's APMs only.
	usage *usageCollector

	// handles is owned by the loop goroutine.
	handles map[string]*policyHandle
	queue   []string

	workCh   chan *compiledPolicy
	resultCh chan evalResult
}

func newRunner(logger hclog.Logger, cfg *Config, deps *Deps, now func() time.Time, host *pluginHost) *runner {
	return &runner{
		logger: logger,
		cfg:    cfg,
		deps:   deps,
		now:    now,
		host:   host,
		proc: &processor{
			defaultCooldown:           cfg.DefaultCooldown,
			defaultEvaluationInterval: cfg.DefaultEvaluationInterval,
			minEvaluationInterval:     cfg.MinEvalInterval,
			nomadAPMs:                 host.nomadAPMs,
		},
		exec:     newExecutor(logger.Named("executor"), cfg, deps.Scaler, deps.ObjectScaler, deps.PublishEvent),
		handles:  map[string]*policyHandle{},
		workCh:   make(chan *compiledPolicy),
		resultCh: make(chan evalResult),
	}
}

// run schedules the policy evaluations until ctx is canceled.
func (r *runner) run(ctx context.Context) {
	for i := 0; i < r.cfg.EvalWorkers; i++ {
		go r.worker(ctx)
	}

	updatesCh := make(chan *policyList, 1)
	go r.watchPolicies(ctx, updatesCh)

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C

	for {
		// Dispatch the head of the queue only when a worker is free, by
		// enabling the send case.
		var sendCh chan *compiledPolicy
		var next *compiledPolicy
		if len(r.queue) > 0 {
			sendCh = r.workCh
			next = r.handles[r.queue[0]].compiled
		}

		r.resetTimer(timer)

		select {
		case <-ctx.Done():
			return

		case list := <-updatesCh:
			r.reconcile(list, r.now())
			r.enqueueDue(r.now())

		case <-timer.C:
			r.enqueueDue(r.now())

		case sendCh <- next:
			h := r.handles[r.queue[0]]
			h.queued = false
			h.inFlight = true
			r.queue = r.queue[1:]

		case res := <-r.resultCh:
			if res.err != nil {
				r.logger.Error("policy evaluation failed", "policy_id", res.id, "error", res.err)
			}
			if h, ok := r.handles[res.id]; ok {
				h.inFlight = false
				if h.compiled != nil {
					h.nextEval = r.now().Add(h.compiled.policy.EvaluationInterval)
				}
			}
		}
	}
}

// resetTimer arms the timer for the earliest scheduled evaluation.
func (r *runner) resetTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}

	var earliest time.Time
	for _, h := range r.handles {
		if h.compiled == nil || h.queued || h.inFlight {
			continue
		}
		if earliest.IsZero() || h.nextEval.Before(earliest) {
			earliest = h.nextEval
		}
	}
	if earliest.IsZero() {
		// Nothing to schedule: wait for policy updates.
		timer.Reset(time.Hour)
		return
	}
	d := earliest.Sub(r.now())
	if d < 0 {
		d = 0
	}
	timer.Reset(d)
}

// enqueueDue queues the policies whose evaluation is due.
func (r *runner) enqueueDue(now time.Time) {
	for id, h := range r.handles {
		if h.compiled == nil || h.queued || h.inFlight || h.nextEval.After(now) {
			continue
		}
		h.queued = true
		r.queue = append(r.queue, id)
	}
	metrics.SetGauge([]string{"nomad", "autoscaler", "queue"}, float32(len(r.queue)))
}

// worker evaluates the policies sent by the runner loop.
func (r *runner) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-r.workCh:
			err := r.evaluate(ctx, c)
			select {
			case <-ctx.Done():
				return
			case r.resultCh <- evalResult{id: c.policy.ID, err: err}:
			}
		}
	}
}

// evaluate runs one evaluation of a policy: check → strategy → proposed
// count, and hands the decision to the executor.
func (r *runner) evaluate(ctx context.Context, c *compiledPolicy) error {
	defer metrics.MeasureSince([]string{"nomad", "autoscaler", "evaluate"}, time.Now())

	logger := r.logger.With("policy_id", c.policy.ID, "target", c.key.String())
	now := r.now()

	if c.schedule != nil && !c.schedule.activeAt(now) {
		logger.Debug("skipping evaluation, outside schedule window")
		return nil
	}

	store := r.deps.State()
	var t *targetState
	var err error
	if c.object != nil {
		logger = logger.With("object", c.object.String())
		t, err = readObjectTarget(store, c.key, c.object)
	} else {
		t, err = readTarget(store, c.key)
	}
	if err != nil {
		return fmt.Errorf("failed to read target: %w", err)
	}

	// A nil target indicates the job doesn't exist; the policy goes away
	// with it.
	if t == nil {
		logger.Debug("skipping evaluation", "reason", errTargetNotFound)
		return nil
	}

	if !t.status.Ready {
		logger.Debug("skipping evaluation, target not ready")
		return nil
	}

	// No scaling during an active deployment, unless the policy allows
	// scaling up during deployments and no canary waits for promotion.
	upOnly := false
	if t.deploymentActive {
		if c.scaleDuringDeployment != structs.ScaleDuringDeploymentUpOnly || t.unpromotedCanary {
			metrics.IncrCounter([]string{"nomad", "autoscaler", "evaluate", "deployment"}, 1)
			logger.Debug("skipping evaluation, deployment in progress", "unpromoted_canary", t.unpromotedCanary)
			return nil
		}
		upOnly = true
	}

	// Policies are not evaluated during cooldown, as defined by the
	// autoscaler's policy documentation.
	if until, reason := r.exec.blockedUntil(c.key, c.policy, t); now.Before(until) {
		metrics.IncrCounter([]string{"nomad", "autoscaler", "evaluate", "skipped"}, 1)
		logger.Debug("skipping evaluation", "reason", reason, "remaining", until.Sub(now))
		return nil
	}

	currentCount := t.status.Count
	action, check, err := r.calculateNewCount(ctx, c, currentCount)
	if err != nil {
		return fmt.Errorf("unable to execute policy: %w", err)
	}

	logger.Debug("calculated scaling target", "from", currentCount, "to", action.Count,
		"reason", action.Reason, "meta", action.Meta)

	if !scalingNeeded(action, currentCount) {
		logger.Debug("skipping scaling, no action needed")
		return nil
	}

	if upOnly && action.Direction != sdk.ScaleDirectionUp {
		logger.Debug("skipping scaling, only scaling up is allowed during the deployment")
		return nil
	}

	// Scaling up is capped at the quota ceiling of the namespace. An
	// object target is capped with the task group of its policy as the
	// size of one replica, if the group exists.
	if action.Direction == sdk.ScaleDirectionUp && t.group != nil {
		ok, err := r.capAtQuota(store, c, t, &action, check, now)
		if err != nil {
			return fmt.Errorf("failed to check the quota: %w", err)
		}
		if !ok {
			return nil
		}
	}

	// The policy may have been disabled or moved to the external engine
	// while it was evaluated. Never act on it in that case, to avoid scaling
	// a group by two systems.
	if !r.policyStillActive(c) {
		logger.Debug("skipping scaling, policy is no longer active")
		return nil
	}

	return r.exec.execute(c, t, action, check, r.now())
}

// scalingNeeded determines if the action requires a change from the current
// count.
func scalingNeeded(a sdk.ScalingAction, currentCount int64) bool {
	if a.Direction == sdk.ScaleDirectionNone || a.Direction == sdk.ScaleDirectionRecommendation {
		return false
	}
	return a.Count != currentCount
}

// calculateNewCount is the main part of the controller, it
// gets the metrics and the necessary new count to keep up with the policy
// and generates a scaling action if needed.
func (r *runner) calculateNewCount(ctx context.Context, c *compiledPolicy, currentCount int64) (sdk.ScalingAction, string, error) {
	// Store check results by group so we can compare their results together.
	checkGroups := make(map[string][]checkResult)
	queryCache := newQueryMetricsCache()

	for _, ch := range c.checkRunners {
		action, err := ch.runCheckAndCapCount(ctx, currentCount, queryCache)
		if err != nil {
			if errors.Is(err, errCheckOutsideSchedule) || errors.Is(err, errCheckSkipped) {
				continue
			}
			return sdk.ScalingAction{}, "", fmt.Errorf("failed to run check and cap count: %w", err)
		}

		g := ch.group()
		checkGroups[g] = append(checkGroups[g], checkResult{
			action:  &action,
			handler: ch,
			group:   g,
		})
	}

	winner := pickWinnerActionFromGroups(checkGroups)
	if winner.handler == nil || winner.action == nil {
		return sdk.ScalingAction{}, "", nil
	}

	return *winner.action, winner.handler.name(), nil
}

// pickWinnerActionFromGroups decide which action wins in the group.
// The decision processes still picks the safest choice, but it handles `none`
// actions a little differently.
//
// Since grouped checks have corelated metrics, it's expected that most
// checks will result in `none` actions as the data will be somewhere
// else. So we ignore none actions unless _all_ checks in the group
// vote for `none` to avoid accidentally scaling down when comparing
// with other groups.
func pickWinnerActionFromGroups(checkGroups map[string][]checkResult) checkResult {
	var winner checkResult
	for group, results := range checkGroups {

		var groupWinner checkResult

		noneCount := 0
		for _, r := range results {
			if r.action == nil {
				continue
			}

			if group != "" && r.action.Direction == sdk.ScaleDirectionNone {
				noneCount += 1
				continue
			}
			groupWinner = groupWinner.preempt(r)
		}

		// If all checks result in `none`, pick any one of them so when we
		// don't scale down accidentally when comparing it with other groups.
		if noneCount > 0 && noneCount == len(results) {
			groupWinner = results[0]
		}

		if groupWinner.handler == nil {
			continue
		}

		winner = winner.preempt(groupWinner)
	}

	return winner
}

type checkResult struct {
	action  *sdk.ScalingAction
	handler checker
	group   string
}

func (c checkResult) preempt(other checkResult) checkResult {
	winner := sdk.PreemptScalingAction(c.action, other.action)
	if winner == c.action {
		return c
	}
	return other
}

// loadCheckRunners creates the check runners of a policy.
func (r *runner) loadCheckRunners(policy *sdk.ScalingPolicy) ([]checker, error) {
	runners := []checker{}

	for i, check := range policy.Checks {
		if check == nil {
			return nil, fmt.Errorf("invalid check at index %d: check cannot be nil", i)
		}

		if check.Strategy == nil || check.Strategy.Name == "" {
			return nil, fmt.Errorf("invalid check %q: missing strategy value", check.Name)
		}

		strategyName := check.Strategy.Name

		s, err := r.host.getStrategyRunner(strategyName)
		if err != nil {
			return nil, fmt.Errorf("failed to get strategy %s: %w", strategyName, err)
		}

		var mg plugins.Looker
		// Fixed-value strategy does not need APM; keep looker nil and skip APM lookup.
		switch {
		case strategyName == plugins.InternalStrategyFixedValue:
		case r.usage != nil && r.proc.isNomadAPMQuery(check.Source):
			// Nomad's own metrics come from the usage pushed by clients.
			mg = r.usageLooker(policy)
		default:
			mg, err = r.host.getAPMLooker(check.Source)
			if err != nil {
				return nil, fmt.Errorf("failed to get APM for strategy %s: %w", strategyName, err)
			}
		}

		runners = append(runners, newCheckRunner(&CheckRunnerConfig{
			Log: r.logger.Named("check_handler").With("policy_id", policy.ID, "check", check.Name,
				"source", check.Source, "strategy", strategyName),
			StrategyRunner: s,
			MetricsGetter:  mg,
			Policy:         policy,
			Now:            r.now,
		}, check))
	}

	return runners, nil
}

// splay returns a random delay within the evaluation interval, used to
// spread the first evaluations of policies loaded at the same time.
func splay(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(interval)))
}
