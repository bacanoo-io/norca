// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Parts of the check validation are ported from Nomad Autoscaler v0.6.0
// policy/nomad/validate.go (rules for query, query_window = "instant" and
// strategies without metrics), rewritten for the typed scaling policy.

package autoscaler

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/fixedvalue"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/targetvalue"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/threshold"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/structs"
)

// BuiltinMetrics returns the names of the metrics a check can use by name
// (see metrics.go).
func BuiltinMetrics() []string {
	names := slices.Collect(maps.Keys(builtinMetrics))
	sort.Strings(names)
	return names
}

// strategyConfigAliases are short names accepted for strategy parameters in
// the typed check form, keyed by strategy driver.
var strategyConfigAliases = map[string]map[string]string{
	plugins.InternalStrategyThreshold: {
		"upper": "upper_bound",
		"lower": "lower_bound",
	},
}

// strategyConfig returns the strategy parameters of a check for the
// strategy driver, with aliases resolved.
func strategyConfig(driver string, config map[string]string) map[string]string {
	out := maps.Clone(config)
	if out == nil {
		out = map[string]string{}
	}
	for alias, key := range strategyConfigAliases[driver] {
		if v, ok := out[alias]; ok {
			if _, set := out[key]; !set {
				out[key] = v
			}
			delete(out, alias)
		}
	}
	return out
}

// strategyValidators check the parameters of the built-in strategies.
var strategyValidators = map[string]func(map[string]string) error{
	plugins.InternalStrategyTargetValue: targetvalue.ValidateConfig,
	plugins.InternalStrategyThreshold:   threshold.ValidateConfig,
	plugins.InternalStrategyFixedValue:  fixedvalue.ValidateConfig,
}

// TypedPolicy returns the policy with its typed fields filled from the
// untyped document if they are empty, and the errors of that conversion.
// The input is not modified.
func TypedPolicy(p *structs.ScalingPolicy) (*structs.ScalingPolicy, error) {
	if p.HasTypedPolicy() {
		return p, nil
	}
	tp := p.Copy()
	err := tp.FillTypedPolicy()
	return tp, err
}

// ValidatePolicy validates a task group scaling policy of the builtin engine
// against the configuration of the built-in autoscaler. It is used at job
// submission (job plan and job run) and before a policy is evaluated.
func ValidatePolicy(p *structs.ScalingPolicy, cfg *Config) error {
	return validatePolicy(p, cfg, true)
}

// validatePolicy validates a policy. The lower bounds of the evaluation
// interval and the cooldowns and the existence of the referenced plugins are
// only checked at admission; when a policy is loaded for evaluation the
// interval is raised to the minimum and the plugin host reports missing
// plugins.
func validatePolicy(p *structs.ScalingPolicy, cfg *Config, admission bool) error {
	var mErr *multierror.Error
	if p == nil {
		return nil
	}
	if cfg == nil {
		cfg = DefaultConfig()
	}

	// The errors in the untyped document are only relevant if the typed
	// policy was derived from it.
	tp, err := TypedPolicy(p)
	if err == nil && p.HasTypedPolicy() && len(p.Policy) != 0 {
		derived := &structs.ScalingPolicy{Policy: p.Policy}
		if dErr := derived.FillTypedPolicy(); dErr != nil && derived.TypedPolicyEqual(p) {
			err = dErr
		}
	}
	if err != nil {
		mErr = multierror.Append(mErr, err)
	}

	if tp.Type != "" && tp.Type != structs.ScalingPolicyTypeHorizontal {
		mErr = multierror.Append(mErr, fmt.Errorf("policy type %q is not supported by the builtin engine", tp.Type))
	}
	if tp.Target[structs.ScalingTargetTask] != "" {
		mErr = multierror.Append(mErr, errors.New("task scaling policies are not supported by the builtin engine"))
	}
	if _, err := objectTargetFromPolicy(p); err != nil {
		mErr = multierror.Append(mErr, err)
	}

	if tp.EvaluationInterval < 0 {
		mErr = multierror.Append(mErr, errors.New("policy.evaluation_interval must not be negative"))
	} else if admission && tp.EvaluationInterval != 0 && tp.EvaluationInterval < cfg.MinEvalInterval {
		mErr = multierror.Append(mErr, fmt.Errorf("policy.evaluation_interval must be at least %s, found %s",
			cfg.MinEvalInterval, tp.EvaluationInterval))
	}
	for _, cd := range []struct {
		name  string
		value int64
	}{
		{"cooldown", int64(tp.Cooldown)},
		{"cooldown_on_scale_up", int64(tp.CooldownOnScaleUp)},
	} {
		switch {
		case cd.value < 0:
			mErr = multierror.Append(mErr, fmt.Errorf("policy.%s must not be negative", cd.name))
		case admission && cd.value != 0 && cd.value < int64(cfg.MinCooldown):
			mErr = multierror.Append(mErr, fmt.Errorf("policy.%s must be at least %s, found %s",
				cd.name, cfg.MinCooldown, time.Duration(cd.value)))
		}
	}
	if err := structs.ValidateScalingOnError(tp.OnCheckError); err != nil {
		mErr = multierror.Append(mErr, fmt.Errorf("policy.on_check_error %w", err))
	}
	if err := structs.ValidateScaleDuringDeployment(tp.ScaleDuringDeployment); err != nil {
		mErr = multierror.Append(mErr, fmt.Errorf("policy.scale_during_deployment %w", err))
	}

	if len(tp.Checks) == 0 {
		mErr = multierror.Append(mErr, errors.New("policy must have at least one check"))
	}
	seen := map[string]bool{}
	for _, c := range tp.Checks {
		if c == nil {
			continue
		}
		if seen[c.Name] {
			mErr = multierror.Append(mErr, fmt.Errorf("duplicate check %q", c.Name))
		}
		seen[c.Name] = true
		if err := validateTypedCheck(c, cfg, admission); err != nil {
			mErr = multierror.Append(mErr, err)
		}
	}

	// The schedule is only available in the untyped document.
	if err := validateSchedules(p.Policy); err != nil {
		mErr = multierror.Append(mErr, err)
	}

	return mErr.ErrorOrNil()
}

// validateCheck validates one typed check.
func validateTypedCheck(c *structs.ScalingCheck, cfg *Config, admission bool) error {
	var mErr *multierror.Error
	path := fmt.Sprintf("check[%s]", c.Name)
	if c.Name == "" {
		path = "check"
		mErr = multierror.Append(mErr, errors.New("check must have a name"))
	}

	if err := structs.ValidateScalingOnError(c.OnError); err != nil {
		mErr = multierror.Append(mErr, fmt.Errorf("%s.on_error %w", path, err))
	}
	if c.QueryWindow < 0 || c.QueryWindowOffset < 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("%s: query windows must not be negative", path))
	}

	// Strategy.
	driver := ""
	switch {
	case c.Strategy == "":
		mErr = multierror.Append(mErr, fmt.Errorf("%s: missing strategy", path))
	case admission && !slices.Contains(cfg.KnownStrategies(), c.Strategy):
		mErr = multierror.Append(mErr, fmt.Errorf("%s: unknown strategy %q (available: %s)",
			path, c.Strategy, strings.Join(cfg.KnownStrategies(), ", ")))
	default:
		driver = cfg.StrategyDriver(c.Strategy)
	}
	if validate, ok := strategyValidators[driver]; ok {
		if err := validate(strategyConfig(driver, c.StrategyConfig)); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("%s: strategy %q: %w", path, c.Strategy, err))
		}
	}
	needsMetrics := driver != plugins.InternalStrategyFixedValue

	// Metric source.
	switch {
	case c.Metric != "":
		// Built-in metrics are computed by the leader from the usage pushed
		// by clients and served by the nomad-apm source.
		if !IsBuiltinMetric(c.Metric) {
			mErr = multierror.Append(mErr, fmt.Errorf("%s.metric must be one of %s, found %q",
				path, strings.Join(BuiltinMetrics(), ", "), c.Metric))
		}
		if c.Query != "" {
			mErr = multierror.Append(mErr, fmt.Errorf("%s: only one of metric and query can be set", path))
		}
		if c.Source != "" && !slices.Contains(cfg.nomadAPMNames(), c.Source) {
			mErr = multierror.Append(mErr, fmt.Errorf("%s.metric requires the %s source", path, plugins.InternalAPMNomad))
		}
	case needsMetrics && c.Query == "":
		mErr = multierror.Append(mErr, fmt.Errorf("%s: strategy %q requires a query or a metric", path, c.Strategy))
	}
	if admission && c.Source != "" && !slices.Contains(cfg.KnownSources(), c.Source) {
		mErr = multierror.Append(mErr, fmt.Errorf("%s: unknown source %q (available: %s)",
			path, c.Source, strings.Join(cfg.KnownSources(), ", ")))
	}

	// A threshold check on an instant query must trigger on a single data
	// point.
	if c.QueryInstant && driver == plugins.InternalStrategyThreshold {
		trigger := strategyConfig(driver, c.StrategyConfig)[thresholdWithinBoundsTriggerConfigKey]
		if v, err := strconv.Atoi(trigger); err != nil || v != 1 {
			mErr = multierror.Append(mErr, fmt.Errorf("%s: %q must be set to 1 when query_window = %q",
				path, thresholdWithinBoundsTriggerConfigKey, "instant"))
		}
	}

	return mErr.ErrorOrNil()
}

// validateSchedules validates the schedule blocks of the untyped policy
// document, at policy and check level.
func validateSchedules(doc map[string]interface{}) error {
	var mErr *multierror.Error
	if s := parseSchedule(normalizeBlocks(doc[keySchedule])); s != nil {
		if err := sdk.ValidateScalingPolicySchedule(s); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("policy.schedule: %w", err))
		}
	}
	for name, raw := range parseBlocks(normalizeBlocks(doc[keyChecks])) {
		check := parseBlock(normalizeBlocks(raw))
		if s := parseSchedule(normalizeBlocks(check[keySchedule])); s != nil {
			if err := sdk.ValidateScalingPolicySchedule(s); err != nil {
				mErr = multierror.Append(mErr, fmt.Errorf("check[%s].schedule: %w", name, err))
			}
		}
	}
	return mErr.ErrorOrNil()
}

// normalizeBlocks converts a []map[string]interface{} list, as produced by
// the HCL parsers before the job is encoded, into the []interface{} form the
// document has after JSON or msgpack decoding.
func normalizeBlocks(raw interface{}) interface{} {
	if list, ok := raw.([]map[string]interface{}); ok {
		out := make([]interface{}, len(list))
		for i := range list {
			out[i] = list[i]
		}
		return out
	}
	return raw
}

// applyTypedPolicy sets the fields of the typed policy on a policy parsed
// from the untyped document. The schedule and the target only exist in the
// document; everything else comes from the typed policy when it is set.
func applyTypedPolicy(sp *sdk.ScalingPolicy, tp *structs.ScalingPolicy, cfg *Config) {
	if tp.EvaluationInterval != 0 {
		sp.EvaluationInterval = tp.EvaluationInterval
	}
	if tp.Cooldown != 0 {
		sp.Cooldown = tp.Cooldown
	}
	if tp.CooldownOnScaleUp != 0 {
		sp.CooldownOnScaleUp = tp.CooldownOnScaleUp
	}
	if tp.OnCheckError != "" {
		sp.OnCheckError = tp.OnCheckError
	}
	if len(tp.Checks) == 0 {
		return
	}

	schedules := map[string]*sdk.ScalingPolicySchedule{}
	for _, c := range sp.Checks {
		schedules[c.Name] = c.Schedule
	}

	sp.Checks = make([]*sdk.ScalingPolicyCheck, 0, len(tp.Checks))
	for _, c := range tp.Checks {
		if c == nil {
			continue
		}
		// A built-in metric is queried by name from the nomad-apm source,
		// which the runtime serves from the usage pushed by clients.
		source, query := c.Source, c.Query
		if c.Metric != "" {
			query = c.Metric
			if source == "" {
				source = plugins.InternalAPMNomad
			}
		}
		sp.Checks = append(sp.Checks, &sdk.ScalingPolicyCheck{
			Name:              c.Name,
			Group:             c.Group,
			Source:            source,
			Query:             query,
			QueryWindow:       c.QueryWindow,
			QueryWindowOffset: c.QueryWindowOffset,
			QueryInstant:      c.QueryInstant,
			OnError:           c.OnError,
			Schedule:          schedules[c.Name],
			Strategy: &sdk.ScalingPolicyStrategy{
				Name:   c.Strategy,
				Config: strategyConfig(cfg.StrategyDriver(c.Strategy), c.StrategyConfig),
			},
		})
	}
}
