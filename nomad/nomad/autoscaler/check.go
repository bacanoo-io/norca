// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 policy/check.go.
// Modified: in-process plugin interfaces, the clock is injected for tests,
// metrics use the Nomad go-metrics fork and the nomad.autoscaler prefix
// without per-policy labels, and query errors follow the check's on_error
// setting instead of always aborting the evaluation.

package autoscaler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
)

// errCheckOutsideSchedule signals that a check fell outside its configured
// schedule window and should be skipped from winner selection. The caller in
// the handler treats it as a non-fatal skip; any other error aborts the
// evaluation.
var errCheckOutsideSchedule = errors.New("check is not within defined schedule")

// errCheckSkipped signals that the metrics of a check could not be queried
// and the check is configured to ignore errors. Like errCheckOutsideSchedule,
// the check is left out of the winner selection.
var errCheckSkipped = errors.New("check skipped after a query error")

type CheckRunnerConfig struct {
	// Log is the logger to use for logging.
	Log hclog.Logger

	// StrategyRunner is the strategy runner for the check.
	StrategyRunner plugins.StrategyRunner

	// MetricsGetter is the metrics getter for the check.
	MetricsGetter plugins.Looker

	Policy *sdk.ScalingPolicy

	// Now returns the current time.
	Now func() time.Time
}

// checkHandler evaluates one of the checks of a policy.
type checkRunner struct {
	log            hclog.Logger
	strategyRunner plugins.StrategyRunner
	metricsGetter  plugins.Looker
	check          *sdk.ScalingPolicyCheck
	policy         *sdk.ScalingPolicy
	schedule       *compiledSchedule
	scheduleErr    error
	scheduleOnce   sync.Once
	now            func() time.Time
}

type queryMetricsCacheKey struct {
	source            string
	query             string
	queryWindow       time.Duration
	queryWindowOffset time.Duration
}

type queryMetricsCacheEntry struct {
	metrics sdk.TimestampedMetrics
	err     error
}

type queryMetricsCache struct {
	entries map[queryMetricsCacheKey]queryMetricsCacheEntry
}

func newQueryMetricsCache() *queryMetricsCache {
	return &queryMetricsCache{entries: make(map[queryMetricsCacheKey]queryMetricsCacheEntry)}
}

func cloneMetrics(ms sdk.TimestampedMetrics) sdk.TimestampedMetrics {
	if len(ms) == 0 {
		return nil
	}

	cloned := make(sdk.TimestampedMetrics, len(ms))
	copy(cloned, ms)
	return cloned
}

// NewCheckRunner returns a new checkRunner instance.
func newCheckRunner(config *CheckRunnerConfig, c *sdk.ScalingPolicyCheck) *checkRunner {
	return &checkRunner{
		log:            config.Log,
		check:          c,
		strategyRunner: config.StrategyRunner,
		metricsGetter:  config.MetricsGetter,
		policy:         config.Policy,
		now:            config.Now,
	}
}

func (ch *checkRunner) ensureCompiledSchedule() error {
	ch.scheduleOnce.Do(func() {
		ch.schedule, ch.scheduleErr = compileSchedule(ch.check.Schedule)
	})
	return ch.scheduleErr
}

// getNewCountFromStrategy runs the strategy for one check and returns a
// scaling action, applying check-level and policy-level error handling.
func (ch *checkRunner) getNewCountFromStrategy(ctx context.Context, currentCount int64,
	metrics sdk.TimestampedMetrics) (sdk.ScalingAction, error) {
	ch.log.Debug("calculating new count", "current count", currentCount)

	a, err := ch.runStrategy(ctx, currentCount, metrics)
	if err != nil {
		ch.log.Warn("failed to run check", "check", ch.check.Name,
			"on_error", ch.check.OnError, "on_check_error",
			ch.policy.OnCheckError, "error", err)

		// Use recognized check-level on_error values first.
		// For unset or unrecognized values, fall back to policy on_check_error.
		if ch.failOnError() {
			return sdk.ScalingAction{}, err
		}

		// Non-fatal fallback path: skip this check result and continue evaluating
		// the remaining checks in the policy.
		return sdk.ScalingAction{}, nil
	}

	return a, nil
}

func (ch *checkRunner) runStrategy(ctx context.Context, currentCount int64, ms sdk.TimestampedMetrics) (sdk.ScalingAction, error) {
	checkEval := &sdk.ScalingCheckEvaluation{
		Check: ch.check,
		Action: &sdk.ScalingAction{
			Meta: map[string]interface{}{
				"nomad_policy_id": ch.policy.ID,
			},
		},
		Metrics: ms,
	}

	// Trigger a metric measure to track latency of the call.
	labels := []metrics.Label{
		{Name: "plugin_name", Value: ch.check.Strategy.Name},
	}

	defer metrics.MeasureSinceWithLabels([]string{"nomad", "autoscaler", "strategy", "run"}, time.Now(), labels)

	var runResp *sdk.ScalingCheckEvaluation
	var err error

	ch.log.Debug("running strategy", "strategy", ch.check.Strategy.Name, "check", ch.check.Name)

	// Wrap call in a goroutine so we can listen for ctx as well.
	strategyRunDoneCh := make(chan interface{})
	go func() {
		defer close(strategyRunDoneCh)
		runResp, err = ch.strategyRunner.Run(checkEval, currentCount)
	}()

	select {
	case <-ctx.Done():
		return sdk.ScalingAction{}, ctx.Err()
	case <-strategyRunDoneCh:
	}

	if err != nil {
		return sdk.ScalingAction{}, fmt.Errorf("failed to execute strategy: %w", err)
	}

	if runResp == nil {
		return sdk.ScalingAction{}, nil
	}

	return *runResp.Action, nil
}

// queryMetrics wraps the apm.Query call to provide operational functionality.
func (ch *checkRunner) queryMetrics(ctx context.Context, cache *queryMetricsCache) (sdk.TimestampedMetrics, error) {
	ch.log.Debug("querying source", "query", ch.check.Query, "source", ch.check.Source)

	cacheKey := queryMetricsCacheKey{
		source:            ch.check.Source,
		query:             ch.check.Query,
		queryWindow:       ch.check.QueryWindow,
		queryWindowOffset: ch.check.QueryWindowOffset,
	}

	if cache != nil {
		if entry, ok := cache.entries[cacheKey]; ok {
			ch.log.Trace("using cached query result")
			if entry.err != nil {
				return sdk.TimestampedMetrics{}, entry.err
			}
			return cloneMetrics(entry.metrics), nil
		}
	}

	// Trigger a metric measure to track latency of the call.
	labels := []metrics.Label{{Name: "plugin_name", Value: ch.check.Source}}
	defer metrics.MeasureSinceWithLabels([]string{"nomad", "autoscaler", "apm", "query"},
		time.Now(), labels)

	// Calculate query range from the query window defined in the ch.check.
	to := ch.now().Add(-ch.check.QueryWindowOffset)
	from := to.Add(-ch.check.QueryWindow)
	if ch.check.QueryInstant {
		from = to
	}
	r := sdk.TimeRange{From: from, To: to}

	var err error
	ms := sdk.TimestampedMetrics{}

	// Query check's APM.
	// Wrap call in a goroutine so we can listen for ctx as well.
	apmQueryDoneCh := make(chan interface{})
	go func() {
		defer close(apmQueryDoneCh)
		ms, err = ch.metricsGetter.Query(ch.check.Query, r)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-apmQueryDoneCh:
	}

	if err != nil {
		queryErr := fmt.Errorf("failed to query source: %w", err)
		if cache != nil {
			cache.entries[cacheKey] = queryMetricsCacheEntry{err: queryErr}
		}
		return sdk.TimestampedMetrics{}, queryErr
	}

	if len(ms) == 0 {
		if cache != nil {
			cache.entries[cacheKey] = queryMetricsCacheEntry{err: errNoMetrics}
		}
		return sdk.TimestampedMetrics{}, errNoMetrics
	}

	// Make sure metrics are sorted consistently.
	sort.Sort(ms)

	if cache != nil {
		cache.entries[cacheKey] = queryMetricsCacheEntry{metrics: cloneMetrics(ms)}
	}

	return ms, nil
}

func (ch *checkRunner) group() string {
	return ch.check.Group
}

func (ch *checkRunner) name() string {
	return ch.check.Name
}

// runCheckAndCapCount evaluates the check and caps the returned action.
//
// The method returns errCheckOutsideSchedule when the check is outside its
// schedule window; the caller should skip this check and continue. Any other
// error, including context errors, should abort the evaluation.
func (ch *checkRunner) runCheckAndCapCount(ctx context.Context, currentCount int64, cache *queryMetricsCache) (sdk.ScalingAction, error) {
	ch.log.Debug("received policy check for evaluation")

	if err := ch.ensureCompiledSchedule(); err != nil {
		return sdk.ScalingAction{}, fmt.Errorf("failed to compile check schedule: %w", err)
	}

	if ch.schedule != nil && !ch.schedule.activeAt(ch.now()) {
		return sdk.ScalingAction{}, errCheckOutsideSchedule
	}

	metrics := sdk.TimestampedMetrics{}
	// Fixed-value strategy does not require APM metrics; all other strategies still do.
	if ch.check.Strategy.Name != plugins.InternalStrategyFixedValue {
		var err error
		metrics, err = ch.queryMetrics(ctx, cache)
		if err != nil {
			if ctx.Err() == nil && !ch.failOnError() {
				ch.log.Warn("failed to query source, ignoring check", "error", err)
				return sdk.ScalingAction{}, errCheckSkipped
			}
			return sdk.ScalingAction{}, fmt.Errorf("failed to query source: %w", err)
		}
	}

	action, err := ch.getNewCountFromStrategy(ctx, currentCount, metrics)
	if err != nil {
		return sdk.ScalingAction{}, fmt.Errorf("failed get count from metrics: %w", err)
	}

	action.CapCount(ch.policy.Min, ch.policy.Max)

	return action, nil
}

// failOnError reports whether an error in this check must abort the whole
// policy evaluation. Recognized check-level on_error values are used first;
// for unset or unrecognized values the policy on_check_error applies.
func (ch *checkRunner) failOnError() bool {
	switch ch.check.OnError {
	case sdk.ScalingPolicyOnErrorIgnore:
		return false
	case sdk.ScalingPolicyOnErrorFail:
		return true
	default:
		return ch.policy.OnCheckError == sdk.ScalingPolicyOnErrorFail
	}
}
