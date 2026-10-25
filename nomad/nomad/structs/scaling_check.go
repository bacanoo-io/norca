// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"time"

	multierror "github.com/hashicorp/go-multierror"
)

const (
	// ScalingOnErrorFail and ScalingOnErrorIgnore are the values of
	// ScalingPolicy.OnCheckError and ScalingCheck.OnError.
	ScalingOnErrorFail   = "fail"
	ScalingOnErrorIgnore = "ignore"

	// ScaleDuringDeploymentNever (the default) blocks scaling while a
	// deployment of the job is active. ScaleDuringDeploymentUpOnly allows
	// scaling up during an active deployment, but not scaling down and not
	// while canaries wait for promotion.
	ScaleDuringDeploymentNever  = "never"
	ScaleDuringDeploymentUpOnly = "up_only"
)

// ScalingCheck is one check of a typed scaling policy: the metric it reads
// and the strategy that turns the metric into a desired count.
//
//	check "cpu" {
//	  metric   = "nomad.cpu"
//	  strategy = "target-value"
//	  target   = 70
//	}
type ScalingCheck struct {
	// Name is the label of the check block.
	Name string

	// Source is the APM plugin queried by Query. Empty means the nomad-apm,
	// unless Metric is set.
	Source string

	// Metric is a metric built into the servers (such as nomad.cpu), used
	// instead of Source and Query.
	Metric string

	// Query is the query sent to the APM.
	Query string

	// QueryWindow and QueryWindowOffset define the time range of the query.
	// QueryInstant requests the latest value only (query_window = "instant").
	QueryWindow       time.Duration
	QueryWindowOffset time.Duration
	QueryInstant      bool

	// Group groups checks with correlated metrics.
	Group string

	// Strategy is the strategy plugin and StrategyConfig its parameters.
	Strategy       string
	StrategyConfig map[string]string

	// OnError overrides the policy's OnCheckError for this check.
	OnError string
}

// Copy returns a deep copy of the check.
func (c *ScalingCheck) Copy() *ScalingCheck {
	if c == nil {
		return nil
	}
	nc := *c
	nc.StrategyConfig = maps.Clone(c.StrategyConfig)
	return &nc
}

// Equal reports whether two checks are the same.
func (c *ScalingCheck) Equal(o *ScalingCheck) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c.Name == o.Name &&
		c.Source == o.Source &&
		c.Metric == o.Metric &&
		c.Query == o.Query &&
		c.QueryWindow == o.QueryWindow &&
		c.QueryWindowOffset == o.QueryWindowOffset &&
		c.QueryInstant == o.QueryInstant &&
		c.Group == o.Group &&
		c.Strategy == o.Strategy &&
		maps.Equal(c.StrategyConfig, o.StrategyConfig) &&
		c.OnError == o.OnError
}

func copyScalingChecks(checks []*ScalingCheck) []*ScalingCheck {
	if checks == nil {
		return nil
	}
	out := make([]*ScalingCheck, len(checks))
	for i, c := range checks {
		out[i] = c.Copy()
	}
	return out
}

// HasTypedPolicy reports whether any field of the typed policy is set.
func (p *ScalingPolicy) HasTypedPolicy() bool {
	return p.EvaluationInterval != 0 ||
		p.Cooldown != 0 ||
		p.CooldownOnScaleUp != 0 ||
		p.OnCheckError != "" ||
		p.ScaleDuringDeployment != "" ||
		len(p.Checks) != 0
}

// TypedPolicyEqual reports whether the typed policies of p and o are the
// same.
func (p *ScalingPolicy) TypedPolicyEqual(o *ScalingPolicy) bool {
	if p.EvaluationInterval != o.EvaluationInterval ||
		p.Cooldown != o.Cooldown ||
		p.CooldownOnScaleUp != o.CooldownOnScaleUp ||
		p.OnCheckError != o.OnCheckError ||
		p.ScaleDuringDeployment != o.ScaleDuringDeployment ||
		len(p.Checks) != len(o.Checks) {
		return false
	}
	for i := range p.Checks {
		if !p.Checks[i].Equal(o.Checks[i]) {
			return false
		}
	}
	return true
}

// Keys of the scaling policy document.
const (
	scalingKeyEvaluationInterval    = "evaluation_interval"
	scalingKeyCooldown              = "cooldown"
	scalingKeyCooldownOnScaleUp     = "cooldown_on_scale_up"
	scalingKeyOnCheckError          = "on_check_error"
	scalingKeyScaleDuringDeployment = "scale_during_deployment"
	scalingKeyCheck                 = "check"
	scalingKeySource                = "source"
	scalingKeyMetric                = "metric"
	scalingKeyQuery                 = "query"
	scalingKeyQueryWindow           = "query_window"
	scalingKeyQueryWindowOffset     = "query_window_offset"
	scalingKeyGroup                 = "group"
	scalingKeyStrategy              = "strategy"
	scalingKeyOnError               = "on_error"
	scalingKeySchedule              = "schedule"
)

// scalingCheckKeys are the attributes of a check block that are not
// parameters of the strategy.
var scalingCheckKeys = map[string]bool{
	scalingKeySource:            true,
	scalingKeyMetric:            true,
	scalingKeyQuery:             true,
	scalingKeyQueryWindow:       true,
	scalingKeyQueryWindowOffset: true,
	scalingKeyGroup:             true,
	scalingKeyStrategy:          true,
	scalingKeyOnError:           true,
	scalingKeySchedule:          true,
}

// FillTypedPolicy fills the typed policy from the untyped Policy document
// if no typed field is set. It returns the errors found in the document;
// the fields that could be parsed are filled anyway.
//
// Both forms of a check are understood: the Nomad Autoscaler form with a
// strategy block,
//
//	check "cpu" {
//	  source = "prometheus"
//	  query  = "..."
//	  strategy "target-value" { target = 70 }
//	}
//
// and the typed form, where the strategy is an attribute and its parameters
// are the other attributes of the check:
//
//	check "cpu" {
//	  metric   = "nomad.cpu"
//	  strategy = "target-value"
//	  target   = 70
//	}
func (p *ScalingPolicy) FillTypedPolicy() error {
	if p == nil || p.HasTypedPolicy() || len(p.Policy) == 0 {
		return nil
	}

	var mErr *multierror.Error
	doc := p.Policy

	duration := func(key string, field *time.Duration) {
		raw, ok := doc[key]
		if !ok {
			return
		}
		d, err := scalingDuration(raw)
		if err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("policy.%s: %w", key, err))
			return
		}
		*field = d
	}
	str := func(key string, field *string) {
		raw, ok := doc[key]
		if !ok {
			return
		}
		s, ok := raw.(string)
		if !ok {
			mErr = multierror.Append(mErr, fmt.Errorf("policy.%s must be a string, found %T", key, raw))
			return
		}
		*field = s
	}

	duration(scalingKeyEvaluationInterval, &p.EvaluationInterval)
	duration(scalingKeyCooldown, &p.Cooldown)
	duration(scalingKeyCooldownOnScaleUp, &p.CooldownOnScaleUp)
	str(scalingKeyOnCheckError, &p.OnCheckError)
	str(scalingKeyScaleDuringDeployment, &p.ScaleDuringDeployment)

	if raw, ok := doc[scalingKeyCheck]; ok {
		blocks, err := scalingLabeledBlocks(raw)
		if err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("policy.check: %w", err))
		}
		for _, b := range blocks {
			c, err := parseScalingCheck(b.label, b.body)
			if err != nil {
				mErr = multierror.Append(mErr, err)
			}
			if c != nil {
				p.Checks = append(p.Checks, c)
			}
		}
	}

	return mErr.ErrorOrNil()
}

// parseScalingCheck parses the body of a check block.
func parseScalingCheck(name string, body map[string]interface{}) (*ScalingCheck, error) {
	var mErr *multierror.Error
	path := fmt.Sprintf("policy.check[%s]", name)
	c := &ScalingCheck{Name: name, StrategyConfig: map[string]string{}}

	strs := []struct {
		key   string
		field *string
	}{
		{scalingKeySource, &c.Source},
		{scalingKeyMetric, &c.Metric},
		{scalingKeyQuery, &c.Query},
		{scalingKeyGroup, &c.Group},
		{scalingKeyOnError, &c.OnError},
	}
	for _, s := range strs {
		raw, ok := body[s.key]
		if !ok {
			continue
		}
		v, ok := raw.(string)
		if !ok {
			mErr = multierror.Append(mErr, fmt.Errorf("%s.%s must be a string, found %T", path, s.key, raw))
			continue
		}
		*s.field = v
	}

	if raw, ok := body[scalingKeyQueryWindow]; ok {
		if s, ok := raw.(string); ok && s == "instant" {
			c.QueryInstant = true
		} else if d, err := scalingDuration(raw); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("%s.%s: %w", path, scalingKeyQueryWindow, err))
		} else {
			c.QueryWindow = d
		}
	}
	if raw, ok := body[scalingKeyQueryWindowOffset]; ok {
		if d, err := scalingDuration(raw); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("%s.%s: %w", path, scalingKeyQueryWindowOffset, err))
		} else {
			c.QueryWindowOffset = d
		}
	}

	switch raw := body[scalingKeyStrategy].(type) {
	case nil:
	case string:
		// Typed form: the other attributes are the strategy parameters.
		c.Strategy = raw
		for k, v := range body {
			if scalingCheckKeys[k] {
				continue
			}
			s, err := scalingParam(v)
			if err != nil {
				mErr = multierror.Append(mErr, fmt.Errorf("%s.%s: %w", path, k, err))
				continue
			}
			c.StrategyConfig[k] = s
		}
	default:
		// Nomad Autoscaler form: a labeled strategy block.
		blocks, err := scalingLabeledBlocks(raw)
		switch {
		case err != nil:
			mErr = multierror.Append(mErr, fmt.Errorf("%s.strategy: %w", path, err))
		case len(blocks) != 1:
			mErr = multierror.Append(mErr, fmt.Errorf("%s: expected 1 strategy block, found %d", path, len(blocks)))
		default:
			c.Strategy = blocks[0].label
			for k, v := range blocks[0].body {
				s, err := scalingParam(v)
				if err != nil {
					mErr = multierror.Append(mErr, fmt.Errorf("%s.strategy[%s].%s: %w", path, c.Strategy, k, err))
					continue
				}
				c.StrategyConfig[k] = s
			}
		}
		for k := range body {
			if !scalingCheckKeys[k] {
				mErr = multierror.Append(mErr, fmt.Errorf(
					"%s.%s: strategy parameters must be inside the strategy block", path, k))
			}
		}
	}

	return c, mErr.ErrorOrNil()
}

// scalingDuration parses a duration of the policy document.
func scalingDuration(raw interface{}) (time.Duration, error) {
	s, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("must be a duration string, found %T", raw)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("must have time.Duration format, found %q", s)
	}
	return d, nil
}

// scalingParam converts a strategy parameter to its string form.
func scalingParam(raw interface{}) (string, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprintf("%v", v), nil
	default:
		return "", fmt.Errorf("must be a string, number or bool, found %T", raw)
	}
}

type scalingBlock struct {
	label string
	body  map[string]interface{}
}

// scalingLabeledBlocks flattens a list of labeled blocks of the policy
// document, in the shape produced by the HCL parsers and by JSON decoding:
//
//	[{"label-1": [{...}]}, {"label-2": [{...}]}]
//
// The blocks are sorted by label.
func scalingLabeledBlocks(raw interface{}) ([]scalingBlock, error) {
	var blocks []scalingBlock
	for _, item := range scalingList(raw) {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("must be a list of labeled blocks, found %T", item)
		}
		for label, content := range m {
			if label == "" {
				return nil, errors.New("block must have a label")
			}
			body, err := scalingBlockBody(content)
			if err != nil {
				return nil, fmt.Errorf("block %q: %w", label, err)
			}
			blocks = append(blocks, scalingBlock{label: label, body: body})
		}
	}
	if raw != nil && blocks == nil && scalingList(raw) == nil {
		return nil, fmt.Errorf("must be a list of labeled blocks, found %T", raw)
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].label < blocks[j].label })
	return blocks, nil
}

// scalingBlockBody returns the body of a block: [{...}] or {...}.
func scalingBlockBody(raw interface{}) (map[string]interface{}, error) {
	if m, ok := raw.(map[string]interface{}); ok {
		return m, nil
	}
	list := scalingList(raw)
	switch {
	case list == nil:
		return nil, fmt.Errorf("must be a block, found %T", raw)
	case len(list) == 0:
		return map[string]interface{}{}, nil
	case len(list) != 1:
		return nil, fmt.Errorf("must be a single block, found %d", len(list))
	}
	m, ok := list[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("must be a block, found %T", list[0])
	}
	return m, nil
}

// scalingList returns raw as a list of interface values, or nil if it is
// not a list.
func scalingList(raw interface{}) []interface{} {
	switch v := raw.(type) {
	case []interface{}:
		return v
	case []map[string]interface{}:
		out := make([]interface{}, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	default:
		return nil
	}
}

// ValidateScalingOnError validates an on_check_error or on_error value.
func ValidateScalingOnError(v string) error {
	switch v {
	case "", ScalingOnErrorFail, ScalingOnErrorIgnore:
		return nil
	}
	return fmt.Errorf("must be %q or %q, found %q", ScalingOnErrorFail, ScalingOnErrorIgnore, v)
}

// ValidateScaleDuringDeployment validates a scale_during_deployment value.
func ValidateScaleDuringDeployment(v string) error {
	switch v {
	case "", ScaleDuringDeploymentNever, ScaleDuringDeploymentUpOnly:
		return nil
	}
	return fmt.Errorf("must be %q or %q, found %q",
		ScaleDuringDeploymentNever, ScaleDuringDeploymentUpOnly, v)
}
