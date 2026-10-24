// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// DefaultEvaluationInterval is used when a policy does not set
	// evaluation_interval. Same as the external autoscaler.
	DefaultEvaluationInterval = 10 * time.Second

	// DefaultCooldown is used when a policy does not set cooldown. Same as
	// the external autoscaler.
	DefaultCooldown = 5 * time.Minute

	// DefaultMinEvalInterval is the lower bound enforced on the evaluation
	// interval of builtin policies.
	DefaultMinEvalInterval = 10 * time.Second

	// DefaultEvalWorkers is the number of policy evaluations that may run in
	// parallel on the leader.
	DefaultEvalWorkers = 10

	// DefaultRateLimit and DefaultRateLimitBurst configure the cluster-wide
	// token bucket that limits scaling actions (Job.Scale calls).
	DefaultRateLimit      = 10.0
	DefaultRateLimitBurst = 50

	// DefaultOscillationWindow, DefaultOscillationThreshold and
	// DefaultOscillationSuspend configure the oscillation protection: if a
	// task group changes scaling direction Threshold times within Window, it
	// is suspended for Suspend.
	DefaultOscillationWindow    = 10 * time.Minute
	DefaultOscillationThreshold = 3
	DefaultOscillationSuspend   = 15 * time.Minute

	// DefaultMetricsWindow is how long the leader keeps the task group usage
	// reported by clients (nomad.cpu, nomad.memory, ...).
	DefaultMetricsWindow = 5 * time.Minute

	// DefaultClientMetricsInterval is the aggregation window of the task
	// group usage on the clients, which is also how often they send it.
	DefaultClientMetricsInterval = 10 * time.Second

	// minClientMetricsInterval bounds the client reports to protect the
	// leader.
	minClientMetricsInterval = time.Second
)

// PluginConfig configures a plugin instance, such as an APM.
type PluginConfig struct {
	// Name is the name used by policies to reference the plugin (the source
	// of a check).
	Name string

	// Driver selects the plugin implementation.
	Driver string

	// Config is passed to the plugin's SetConfig.
	Config map[string]string
}

// Copy returns a deep copy of the plugin configuration.
func (p *PluginConfig) Copy() *PluginConfig {
	if p == nil {
		return nil
	}
	np := *p
	if p.Config != nil {
		np.Config = make(map[string]string, len(p.Config))
		for k, v := range p.Config {
			np.Config[k] = v
		}
	}
	return &np
}

// Config is the configuration of the built-in autoscaler. It is set from the
// server's autoscaler block.
type Config struct {
	// Enabled starts the built-in autoscaler on the leader.
	Enabled bool

	// DefaultEngine is the engine of policies that do not set one: "builtin"
	// or "external".
	DefaultEngine string

	// MinEvalInterval is the lower bound of a policy's evaluation interval.
	MinEvalInterval time.Duration

	// DefaultEvaluationInterval and DefaultCooldown are applied to policies
	// that do not set them.
	DefaultEvaluationInterval time.Duration
	DefaultCooldown           time.Duration

	// PluginDir is the directory of external plugins. External plugins are
	// not supported yet; see ExternalPluginLoader.
	PluginDir string

	// EvalWorkers bounds the number of parallel policy evaluations.
	EvalWorkers int

	// RateLimit (actions per second) and RateLimitBurst configure the token
	// bucket on scaling actions.
	RateLimit      float64
	RateLimitBurst int

	// Oscillation protection settings.
	OscillationWindow    time.Duration
	OscillationThreshold int
	OscillationSuspend   time.Duration

	// MetricsWindow is how long the leader keeps the task group usage
	// reported by clients. After a leader change the built-in metrics report
	// insufficient data until one full window has been collected.
	MetricsWindow time.Duration

	// ClientMetricsInterval is the aggregation window of the task group
	// usage on the clients and the interval of their reports.
	ClientMetricsInterval time.Duration

	// APMs configures named APM plugin instances in addition to the always
	// available "nomad-apm".
	APMs []*PluginConfig
}

// DefaultConfig returns the default configuration. The built-in autoscaler
// is disabled and policies default to the external engine, so the behavior
// of existing clusters does not change.
func DefaultConfig() *Config {
	return &Config{
		Enabled:                   false,
		DefaultEngine:             structs.ScalingPolicyEngineExternal,
		MinEvalInterval:           DefaultMinEvalInterval,
		DefaultEvaluationInterval: DefaultEvaluationInterval,
		DefaultCooldown:           DefaultCooldown,
		EvalWorkers:               DefaultEvalWorkers,
		RateLimit:                 DefaultRateLimit,
		RateLimitBurst:            DefaultRateLimitBurst,
		OscillationWindow:         DefaultOscillationWindow,
		OscillationThreshold:      DefaultOscillationThreshold,
		OscillationSuspend:        DefaultOscillationSuspend,
		MetricsWindow:             DefaultMetricsWindow,
		ClientMetricsInterval:     DefaultClientMetricsInterval,
	}
}

// Copy returns a deep copy of the configuration.
func (c *Config) Copy() *Config {
	if c == nil {
		return nil
	}
	nc := *c
	nc.APMs = nil
	for _, a := range c.APMs {
		nc.APMs = append(nc.APMs, a.Copy())
	}
	return &nc
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var mErr *multierror.Error

	switch c.DefaultEngine {
	case structs.ScalingPolicyEngineBuiltin, structs.ScalingPolicyEngineExternal:
	default:
		mErr = multierror.Append(mErr, fmt.Errorf("default_engine must be %q or %q, got %q",
			structs.ScalingPolicyEngineBuiltin, structs.ScalingPolicyEngineExternal, c.DefaultEngine))
	}
	if c.MinEvalInterval <= 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("min_eval_interval must be greater than 0"))
	}
	if c.DefaultEvaluationInterval <= 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("default_evaluation_interval must be greater than 0"))
	}
	if c.DefaultCooldown < 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("default_cooldown must not be negative"))
	}
	if c.EvalWorkers <= 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("eval_workers must be greater than 0"))
	}
	if c.RateLimit <= 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("rate_limit must be greater than 0"))
	}
	if c.RateLimitBurst <= 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("rate_limit_burst must be greater than 0"))
	}
	if c.OscillationWindow <= 0 || c.OscillationThreshold <= 0 || c.OscillationSuspend <= 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("oscillation protection settings must be greater than 0"))
	}
	if c.ClientMetricsInterval < minClientMetricsInterval {
		mErr = multierror.Append(mErr, fmt.Errorf("client_metrics_interval must be at least %s", minClientMetricsInterval))
	}
	if c.MetricsWindow < c.ClientMetricsInterval {
		mErr = multierror.Append(mErr, fmt.Errorf("metrics_window must not be shorter than client_metrics_interval"))
	}

	names := map[string]bool{plugins.InternalAPMNomad: true}
	for _, a := range c.APMs {
		if a.Name == "" {
			mErr = multierror.Append(mErr, fmt.Errorf("apm block must have a name"))
			continue
		}
		if a.Name != plugins.InternalAPMNomad && names[a.Name] {
			mErr = multierror.Append(mErr, fmt.Errorf("duplicate apm %q", a.Name))
		}
		names[a.Name] = true
		switch a.Driver {
		case plugins.InternalAPMNomad, plugins.InternalAPMPrometheus:
		default:
			mErr = multierror.Append(mErr, fmt.Errorf(
				"apm %q: driver %q is not a built-in plugin; external plugins are not supported yet",
				a.Name, a.Driver))
		}
	}

	return mErr.ErrorOrNil()
}

// PolicyEngine returns the engine responsible for the policy: the policy's
// own Engine, or defaultEngine when the policy does not set one.
func PolicyEngine(p *structs.ScalingPolicy, defaultEngine string) string {
	if p.Engine != "" {
		return p.Engine
	}
	if defaultEngine == "" {
		return structs.ScalingPolicyEngineExternal
	}
	return defaultEngine
}
