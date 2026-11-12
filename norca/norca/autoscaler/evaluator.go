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
	"strings"
	"sync/atomic"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	hclog "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics"
)

var (
	// errTargetNotFound is used by a check handler to indicate the policy target
	// does not exist or is unavailable.
	errTargetNotFound = errors.New("target not found")
	errNoMetrics      = errors.New("no metrics available")
)

type checker interface {
	runCheckAndCapCount(ctx context.Context, pop population, cache *queryMetricsCache) (sdk.ScalingAction, checkReading, error)
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
	settle *settleTracker
	status *statusBook

	// usage is the collector of the client pushed task group usage that
	// serves the checks of the nomad-apm source. Nil in tests that use the
	// plugin host's APMs only.
	usage *usageCollector

	// handles is owned by the loop goroutine.
	handles map[string]*policyHandle
	queue   []string

	// queueLen and policyCount are the lengths of queue and handles, for
	// Autoscaler.Stats.
	queueLen    atomic.Int64
	policyCount atomic.Int64

	// lastDemand is the scale last decided for blocked work, by node group,
	// so that a dry run says it once. told is what the blocked work trigger
	// has told about work it can't help, by reason. Both belong to the
	// demand loop.
	lastDemand map[string]int64
	told       map[string]string
	// badWindows is the error last logged for the windows of a group, so
	// that a window that cannot be read is logged once, not every look.
	badWindows map[string]string
	readySeen  map[string]int64
	// unreadySince is when a node group was first seen with fewer than
	// half of its nodes ready, by group (nodegroup_health.go).
	unreadySince map[string]time.Time

	// unneededSince is when a node group was first seen with a node it
	// does not need, by group; lastDown is the scale last decided down, so
	// that a dry run says it once (scaledown.go). A leader change starts the
	// wait again.
	unneededSince map[string]time.Time
	lastDown      map[string]int64

	// balanceLowered is the last lowering of a group for the balance of its
	// pool, and balanceOffUntil until when the balance of a pool is switched
	// off, both by pool (scaledown_balance.go).
	balanceLowered  map[string]balanceMark
	balanceOffUntil map[string]time.Time

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
		exec:    newExecutor(logger.Named("executor"), cfg, deps.Scaler, deps.ObjectScaler, deps.PublishEvent),
		settle:  newSettleTracker(),
		status:  newStatusBook(cfg.HoldWarn, deps.PublishEvent),
		handles: map[string]*policyHandle{},

		lastDemand: map[string]int64{},
		told:       map[string]string{},
		badWindows: map[string]string{},
		readySeen:  map[string]int64{},

		unreadySince: map[string]time.Time{},

		unneededSince: map[string]time.Time{},
		lastDown:      map[string]int64{},

		balanceLowered:  map[string]balanceMark{},
		balanceOffUntil: map[string]time.Time{},
		workCh:          make(chan *compiledPolicy),
		resultCh:        make(chan evalResult),
	}
}

// run schedules the policy evaluations until ctx is canceled.
func (r *runner) run(ctx context.Context) {
	for i := 0; i < r.cfg.EvalWorkers; i++ {
		go r.worker(ctx)
	}

	updatesCh := make(chan *policyList, 1)
	go r.watchPolicies(ctx, updatesCh)
	if r.deps.BlockedDemand != nil || r.deps.GaugeHistory != nil {
		go r.demandLoop(ctx)
	}

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
			r.queueLen.Store(int64(len(r.queue)))

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
	r.queueLen.Store(int64(len(r.queue)))
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
// count, hands the decision to the executor, and records what happened in
// the status book.
func (r *runner) evaluate(ctx context.Context, c *compiledPolicy) error {
	defer metrics.MeasureSince([]string{"nomad", "autoscaler", "evaluate"}, time.Now())

	eval := &Evaluation{Time: r.now()}
	err := r.evaluatePolicy(ctx, c, eval)
	switch {
	case err != nil:
		eval.Outcome, eval.Message = OutcomeError, err.Error()
	case eval.Outcome == "":
		eval.Outcome = OutcomeNone
	}
	// A policy whose target is gone has no status: it goes away with it.
	if eval.Outcome != outcomeTargetGone {
		r.status.record(c.policy.ID, c.key, eval)
	}
	return err
}

// outcomeTargetGone is the outcome of an evaluation that found no target; it
// is not recorded.
const outcomeTargetGone = "target gone"

// evaluatePolicy is the evaluation itself; it fills in eval as it goes. An
// outcome it leaves empty is OutcomeNone.
func (r *runner) evaluatePolicy(ctx context.Context, c *compiledPolicy, eval *Evaluation) error {
	logger := r.logger.With("policy_id", c.policy.ID, "target", c.key.String())
	now := eval.Time

	if c.schedule != nil && !c.schedule.activeAt(now) {
		logger.Debug("skipping evaluation, outside schedule window")
		eval.held(HoldSchedule, "")
		return nil
	}

	store := r.deps.State()

	// The checks of a node group are capped by its bounds now: spec.min and
	// spec.max with the active windows of spec.schedule.
	if c.key.kind == targetKindNodeGroup {
		if obj, err := store.CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", c.key.job); err == nil && obj != nil {
			b, _ := nodeGroupBounds(obj.Spec, now) // a bad window is logged by the demand loop
			c.policy.Min, c.policy.Max = b.min, b.max
		}
	}

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
		eval.Outcome = outcomeTargetGone
		return nil
	}
	eval.Desired, eval.Running = t.desired, t.status.Count

	if !t.status.Ready {
		logger.Debug("skipping evaluation, target not ready")
		eval.held(HoldNotReady, "")
		return nil
	}

	// Since when the target does not run what was asked for: it holds the
	// scale downs below.
	isSettled := t.isSettled()
	unsettledFor := r.settle.observe(c.key, isSettled, now)

	// No scaling during an active deployment, unless the policy allows
	// scaling up during deployments and no canary waits for promotion.
	upOnly := false
	if t.deploymentActive {
		if c.scaleDuringDeployment != structs.ScaleDuringDeploymentUpOnly || t.unpromotedCanary {
			metrics.IncrCounter([]string{"nomad", "autoscaler", "evaluate", "deployment"}, 1)
			logger.Debug("skipping evaluation, deployment in progress", "unpromoted_canary", t.unpromotedCanary)
			switch {
			case t.unpromotedCanary:
				eval.held(HoldCanary, "canaries of the deployment wait for promotion")
			case t.deploymentWaits:
				eval.held(HoldMultiregion, "the deployment waits for its turn in the multiregion rollout")
			default:
				eval.held(HoldDeployment, "a deployment of the job runs")
			}
			return nil
		}
		upOnly = true
	}

	// Policies are not evaluated during cooldown, as defined by the
	// autoscaler's policy documentation.
	if until, reason := r.exec.blockedUntil(c.key, c.policy, t); now.Before(until) {
		metrics.IncrCounter([]string{"nomad", "autoscaler", "evaluate", "skipped"}, 1)
		logger.Debug("skipping evaluation", "reason", reason, "remaining", until.Sub(now))
		hold := HoldCooldown
		if reason != "cooldown" {
			hold = HoldSuspended
		}
		eval.held(hold, "until "+until.UTC().Format(time.RFC3339))
		return nil
	}

	// The checks say what the load needs against the count that was asked
	// for (desired), not against what happens to run at this moment: while
	// allocations start, wait for capacity, migrate or are replaced, the
	// two differ, and a result taken against the running count would ask
	// again for what is already on its way, or take it back.
	pop := population{desired: t.desired, running: t.status.Count, average: t.runningAverage}
	action, check, err := r.calculateNewCount(ctx, c, pop, eval)
	if err != nil {
		return fmt.Errorf("unable to execute policy: %w", err)
	}

	logger.Debug("calculated scaling target", "desired", t.desired, "running", t.status.Count,
		"to", action.Count, "reason", action.Reason, "meta", action.Meta)

	if !scalingNeeded(action, t.desired) {
		logger.Debug("skipping scaling, no action needed")
		return nil
	}

	// The direction is where the result lies from the desired count; the
	// strategy's own may differ after the count was capped.
	action.Direction = sdk.ScaleDirectionUp
	if action.Count < t.desired {
		action.Direction = sdk.ScaleDirectionDown
	}
	eval.Count, eval.Direction = action.Count, action.Direction.String()

	if upOnly && action.Direction != sdk.ScaleDirectionUp {
		logger.Debug("skipping scaling, only scaling up is allowed during the deployment")
		eval.held(HoldDeployment, "only scaling up is allowed during the deployment")
		return nil
	}

	// A target that is not settled is not scaled down: what it shows is the
	// load on fewer instances than it will have, and the capacity is coming
	// (back). The hold ends with the settle timeout, so that a count that
	// can never run does not keep the target large for good.
	if action.Direction == sdk.ScaleDirectionDown && !isSettled && unsettledFor < r.cfg.SettleTimeout {
		metrics.IncrCounter([]string{"nomad", "autoscaler", "evaluate", "settling"}, 1)
		logger.Debug("skipping scale down, the target is not settled", "desired", t.desired,
			"running", t.status.Count, "migrating", t.isMigrating, "for", unsettledFor)
		eval.held(HoldSettling, fmt.Sprintf("%d of %d run, not settled for %s",
			t.status.Count, t.desired, unsettledFor.Round(time.Second)))
		return nil
	}

	// A node group is lowered only by its scale_down block (scaledown.go),
	// which keeps the protections of a scale down; its checks only raise it.
	if action.Direction == sdk.ScaleDirectionDown && strings.HasPrefix(c.policy.ID, nodeGroupPolicyPrefix) {
		logger.Debug("skipping scaling, a node group is lowered by its scale_down block only")
		eval.held(HoldNodeGroup, "")
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
			eval.held(HoldQuota, "the quota of the namespace has no room for one more")
			return nil
		}
	}

	// The policy may have been disabled or moved to the external engine
	// while it was evaluated. Never act on it in that case, to avoid scaling
	// a group by two systems.
	if !r.policyStillActive(c) {
		logger.Debug("skipping scaling, policy is no longer active")
		eval.held(HoldPolicy, "")
		return nil
	}

	hold, err := r.exec.execute(c, t, action, check, r.now())
	switch {
	case err != nil:
		return err
	case hold != "":
		eval.held(hold, "")
	default:
		eval.Outcome, eval.Count = OutcomeScaled, action.Count
	}
	return nil
}

// scalingNeeded determines if the action requires a change from the count
// that was asked for.
func scalingNeeded(a sdk.ScalingAction, desired int64) bool {
	if a.Direction == sdk.ScaleDirectionNone || a.Direction == sdk.ScaleDirectionRecommendation {
		return false
	}
	return a.Count != desired
}

// calculateNewCount is the main part of the controller, it
// gets the metrics and the necessary new count to keep up with the policy
// and generates a scaling action if needed.
func (r *runner) calculateNewCount(ctx context.Context, c *compiledPolicy, pop population, eval *Evaluation) (sdk.ScalingAction, string, error) {
	// Store check results by group so we can compare their results together.
	checkGroups := make(map[string][]checkResult)
	queryCache := newQueryMetricsCache()

	for _, ch := range c.checkRunners {
		action, reading, err := ch.runCheckAndCapCount(ctx, pop, queryCache)
		switch {
		case errors.Is(err, errCheckOutsideSchedule):
			continue
		case errors.Is(err, errCheckSkipped):
			// Its source gave no value and the check is set to be left out.
			eval.Checks = append(eval.Checks, CheckEvaluation{Name: ch.name(), Direction: checkNoData})
			continue
		case err != nil:
			return sdk.ScalingAction{}, "", fmt.Errorf("failed to run check and cap count: %w", err)
		}
		eval.Checks = append(eval.Checks, CheckEvaluation{
			Name:      ch.name(),
			Value:     reading.value,
			HasValue:  reading.isSet,
			Direction: action.Direction.String(),
			Count:     action.Count,
		})

		g := ch.group()
		checkGroups[g] = append(checkGroups[g], checkResult{
			action:  &action,
			handler: ch,
			group:   g,
		})
	}

	winner := pickWinnerActionFromGroups(checkGroups)
	if winner.handler == nil || winner.action == nil {
		// No check voted. When that is because none had a value, the
		// policy is held, not content.
		for _, check := range eval.Checks {
			if check.Direction == checkNoData {
				eval.held(HoldNoData, "no check has a value")
				break
			}
		}
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

		// A value of an APM is taken as one per instance, measured on what
		// runs now, as target-value has always done; what is known to be
		// otherwise is set below.
		var mg plugins.Looker
		scope := valueScopeRunning
		// Fixed-value strategy does not need APM; keep looker nil and skip APM lookup.
		switch {
		case strategyName == plugins.InternalStrategyFixedValue:
			scope = valueScopeUnknown
		case r.usage != nil && r.proc.isNomadAPMQuery(check.Source):
			// Nomad's own metrics come from the usage pushed by clients.
			usage := r.usageLooker(policy)
			mg, scope = usage, usage.scopeOf(check.Query)
		case check.Source == plugins.InternalAPMNorca:
			// The capacity of the cluster comes from the gauges of the
			// servers. For a node group it is the use of its nodes; for
			// other targets a value of something else.
			mg = r.clusterLooker(policy)
			if !strings.HasPrefix(policy.ID, nodeGroupPolicyPrefix) {
				scope = valueScopeUnknown
			}
		default:
			mg, err = r.host.getAPMLooker(check.Source)
			if err != nil {
				return nil, fmt.Errorf("failed to get APM for strategy %s: %w", strategyName, err)
			}
		}
		// A value handed on as the count is not a value per instance.
		if strategyName == plugins.InternalStrategyPassThrough {
			scope = valueScopeUnknown
		}

		runners = append(runners, newCheckRunner(&CheckRunnerConfig{
			Log: r.logger.Named("check_handler").With("policy_id", policy.ID, "check", check.Name,
				"source", check.Source, "strategy", strategyName),
			StrategyRunner: s,
			MetricsGetter:  mg,
			Policy:         policy,
			Scope:          scope,
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
