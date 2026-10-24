// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 policy/policy.go and the
// canonicalization part of policy/nomad/source.go. Modified: node pool
// (cluster) handling and the gRPC timeout of target plugins were dropped,
// since the built-in autoscaler only scales task groups through the internal
// Job.Scale path.

package autoscaler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/apm/nomadapm"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
)

// DefaultQueryWindow is the value used if `query_window` is not specified in
// a policy check.
const DefaultQueryWindow = time.Minute

const thresholdWithinBoundsTriggerConfigKey = "within_bounds_trigger"

// processor helps process policies and perform common actions on them when
// they are discovered from their source.
type processor struct {
	defaultCooldown           time.Duration
	defaultEvaluationInterval time.Duration
	minEvaluationInterval     time.Duration
	nomadAPMs                 []string
}

// canonicalizePolicy sets standardized values for missing fields.
func (pr *processor) canonicalizePolicy(p *sdk.ScalingPolicy) {
	if p == nil {
		return
	}

	// Assume a policy coming from Nomad without a type is a horizontal policy.
	if p.Type == "" {
		p.Type = sdk.ScalingPolicyTypeHorizontal
	}

	// Apply the cooldown and evaluation interval defaults if the operator did
	// not pass any values.
	pr.applyPolicyDefaults(p)

	// Set default values for Target.
	if p.Target == nil {
		p.Target = &sdk.ScalingPolicyTarget{}
	}

	if p.Target.Config == nil {
		p.Target.Config = make(map[string]string)
	}

	if p.Target.Name == "" {
		p.Target.Name = plugins.InternalTargetNomad
	}

	for _, c := range p.Checks {
		// Set default values for Strategy.
		if c.Strategy == nil {
			c.Strategy = &sdk.ScalingPolicyStrategy{}
		}

		if c.Strategy.Config == nil {
			c.Strategy.Config = make(map[string]string)
		}

		pr.canonicalizeCheck(c, p.Target)
	}
}

// applyPolicyDefaults applies the config defaults to the policy where the
// operator does not supply the parameter.
func (pr *processor) applyPolicyDefaults(p *sdk.ScalingPolicy) {
	if p.Cooldown == 0 {
		p.Cooldown = pr.defaultCooldown
	}

	if p.CooldownOnScaleUp == 0 {
		p.CooldownOnScaleUp = p.Cooldown
	}

	if p.EvaluationInterval == 0 {
		p.EvaluationInterval = pr.defaultEvaluationInterval
	}

	// The server enforces a lower bound on the evaluation interval to protect
	// the leader.
	if p.EvaluationInterval < pr.minEvaluationInterval {
		p.EvaluationInterval = pr.minEvaluationInterval
	}

	for i := 0; i < len(p.Checks); i++ {
		c := p.Checks[i]
		if c.QueryWindow == 0 {
			c.QueryWindow = DefaultQueryWindow
		}
	}
}

// validatePolicy performs validation of the policy document returning a list
// of errors found, if any.
func (pr *processor) validatePolicy(p *sdk.ScalingPolicy) error {

	var mErr *multierror.Error

	if p.ID == "" {
		mErr = multierror.Append(mErr, errors.New("policy ID is empty"))
	}
	if p.Min < 0 {
		mErr = multierror.Append(mErr, errors.New("policy Min can't be negative"))
	}
	if p.Max < 0 {
		mErr = multierror.Append(mErr, errors.New("policy Max can't be negative"))
	}
	if p.Min > p.Max {
		mErr = multierror.Append(mErr, errors.New("policy Min must not be greater Max"))
	}

	// The built-in autoscaler executes scaling actions through the internal
	// Job.Scale path, so only the Nomad target is supported.
	if p.Target != nil && p.Target.Name != plugins.InternalTargetNomad {
		mErr = multierror.Append(mErr, fmt.Errorf(
			"target %q is not supported by the built-in autoscaler", p.Target.Name))
	}

	for _, c := range p.Checks {
		if c == nil {
			continue
		}

		// Threshold strategy with instant queries validation.
		if c.QueryInstant && c.Strategy != nil && c.Strategy.Name == plugins.InternalStrategyThreshold {
			triggerRaw, ok := c.Strategy.Config[thresholdWithinBoundsTriggerConfigKey]
			if !ok {
				mErr = multierror.Append(mErr,
					fmt.Errorf("check %q: %q must be set to 1 when query_window = %q",
						c.Name, thresholdWithinBoundsTriggerConfigKey, "instant"))
				continue
			}

			triggerValue, err := strconv.Atoi(triggerRaw)
			if err != nil || triggerValue != 1 {
				mErr = multierror.Append(mErr,
					fmt.Errorf("check %q: %q must be set to 1 when query_window = %q",
						c.Name, thresholdWithinBoundsTriggerConfigKey, "instant"))
			}
		}
	}

	return mErr.ErrorOrNil()
}

// canonicalizeCheck sets standardised values on fields.
func (pr *processor) canonicalizeCheck(c *sdk.ScalingPolicyCheck, t *sdk.ScalingPolicyTarget) {

	// Operators can omit the check query source which defaults to the Nomad
	// APM.
	if c.Source == "" {
		c.Source = plugins.InternalAPMNomad
	}
	pr.canonicalizeAPMQuery(c, t)
}

// canonicalizeAPMQuery takes a short styled Nomad APM check query and creates
// its fully hydrated internal representation. This is required by the Nomad
// APM if it is being used as the source. The function can be called without
// any validation on the check.
func (pr *processor) canonicalizeAPMQuery(c *sdk.ScalingPolicyCheck, t *sdk.ScalingPolicyTarget) {

	// Catch nils so this function is safe to call without any prior checks.
	if c == nil || t == nil {
		return
	}

	// If the query source is not a Nomad APM, we do not have any additional
	// work to perform. The APM canonicalization is specific to the Nomad APM.
	if !pr.isNomadAPMQuery(c.Source) {
		return
	}

	// If the query already contains a slash it is in full internal form and
	// does not need further processing.
	if !isShortQuery(c.Query) {
		return
	}

	// If the target is a Nomad job task group, format the query in the
	// expected manner.
	if t.IsJobTaskGroupTarget() {
		c.Query = fmt.Sprintf(
			"%s_%s/%s/%s@%s",
			nomadapm.QueryTypeTaskGroup,
			c.Query,
			t.Config[sdk.TargetConfigKeyTaskGroup],
			t.Config[sdk.TargetConfigKeyJob],
			t.Config[sdk.TargetConfigKeyNamespace],
		)
	}
}

// isNomadAPMQuery helps identify whether the policy query is aligned with a
// configured Nomad APM source.
func (pr *processor) isNomadAPMQuery(source string) bool {
	for _, name := range pr.nomadAPMs {
		if source == name {
			return true
		}
	}
	return false
}

// isShortQuery detects if a query is in the <type>_<op>_<metric> format which
// is required by the Nomad APM.
func isShortQuery(q string) bool {
	opMetric := strings.SplitN(q, "_", 2)
	hasSlash := strings.Contains(q, "/")
	return len(opMetric) == 2 && !hasSlash
}
