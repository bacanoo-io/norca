// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	multierror "github.com/hashicorp/go-multierror"
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

	// DefaultMinCooldown is the lower bound enforced on the cooldowns of
	// builtin policies at submission.
	DefaultMinCooldown = 10 * time.Second

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

// DefaultAdviceInterval is how often the advice mode looks at the blocked
// work.
const DefaultAdviceInterval = 30 * time.Second

// AdviceGroup is a pool of nodes in a datacenter that the advice mode may
// advise to grow, with the allocatable size of one of its nodes (CPU in MHz,
// memory and disk in MB, after the reserved resources).
type AdviceGroup struct {
	Name       string
	Pool       string
	Datacenter string
	Size       NodeSize
}

// equal reports whether two advice groups are the same.
func (g *AdviceGroup) equal(o *AdviceGroup) bool {
	return *g == *o
}

// PluginConfig configures a plugin instance, such as an APM.
type PluginConfig struct {
	// Name is the name used by policies to reference the plugin (the source
	// of a check).
	Name string

	// Driver selects the plugin implementation.
	Driver string

	// Args are the command line arguments of an external plugin binary.
	Args []string

	// Config is passed to the plugin's SetConfig.
	Config map[string]string
}

// Copy returns a deep copy of the plugin configuration.
func (p *PluginConfig) Copy() *PluginConfig {
	if p == nil {
		return nil
	}
	np := *p
	np.Args = slices.Clone(p.Args)
	np.Config = maps.Clone(p.Config)
	return &np
}

// equal reports whether two plugin configurations are the same.
func (p *PluginConfig) equal(o *PluginConfig) bool {
	return p.Name == o.Name && p.Driver == o.Driver &&
		slices.Equal(p.Args, o.Args) && maps.Equal(p.Config, o.Config)
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

	// MinCooldown is the lower bound of a policy's cooldown and
	// cooldown_on_scale_up.
	MinCooldown time.Duration

	// DefaultEvaluationInterval and DefaultCooldown are applied to policies
	// that do not set them.
	DefaultEvaluationInterval time.Duration
	DefaultCooldown           time.Duration

	// PluginDir is the directory of the external plugin binaries: APM and
	// strategy plugins built with the Nomad Autoscaler plugin SDK.
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

	// Strategies configures named strategy plugin instances in addition to
	// the always available built-in strategies.
	Strategies []*PluginConfig

	// Advice lists the node groups the read-only advice mode looks at. The
	// mode raises events ("would add 2 nodes") and changes nothing. It runs
	// when the list is not empty.
	Advice         []*AdviceGroup
	AdviceInterval time.Duration

	// NodeScaling are the settings of node scaling that concern the groups
	// of a pool together, or the whole region.
	NodeScaling *NodeScalingConfig
}

// DefaultConfig returns the default configuration. The built-in autoscaler
// is disabled and policies default to the external engine, so the behavior
// of existing clusters does not change.
func DefaultConfig() *Config {
	return &Config{
		Enabled:                   false,
		DefaultEngine:             structs.ScalingPolicyEngineExternal,
		MinEvalInterval:           DefaultMinEvalInterval,
		MinCooldown:               DefaultMinCooldown,
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
		AdviceInterval:            DefaultAdviceInterval,
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
	nc.Strategies = nil
	for _, s := range c.Strategies {
		nc.Strategies = append(nc.Strategies, s.Copy())
	}
	if c.NodeScaling != nil {
		ns := *c.NodeScaling
		ns.Pools = map[string]*NodePoolScaling{}
		for name, p := range c.NodeScaling.Pools {
			np := *p
			ns.Pools[name] = &np
		}
		nc.NodeScaling = &ns
	}
	nc.Advice = nil
	for _, g := range c.Advice {
		ng := *g
		nc.Advice = append(nc.Advice, &ng)
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
	if c.MinCooldown < 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("min_cooldown must not be negative"))
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

	if ns := c.NodeScaling; ns != nil {
		if !validExpander(ns.Expander) {
			mErr = multierror.Append(mErr, fmt.Errorf("node_scaling.expander must be spread, fill, priority or weighted, got %q", ns.Expander))
		}
		if ns.Interval < 0 {
			mErr = multierror.Append(mErr, fmt.Errorf("node_scaling.interval must not be negative"))
		}
		for name, p := range ns.Pools {
			if !validExpander(p.Expander) {
				mErr = multierror.Append(mErr, fmt.Errorf("node_scaling.pool %q: expander must be spread, fill, priority or weighted, got %q", name, p.Expander))
			}
			if p.MaxNodes < 0 || p.MaxStep < 0 {
				mErr = multierror.Append(mErr, fmt.Errorf("node_scaling.pool %q: max_nodes and max_step must not be negative", name))
			}
		}
	}
	if len(c.Advice) > 0 && c.AdviceInterval <= 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("advice_interval must be greater than 0"))
	}
	seen := map[[2]string]bool{}
	for _, g := range c.Advice {
		switch {
		case g.Pool == "" || g.Datacenter == "":
			mErr = multierror.Append(mErr, fmt.Errorf("advice %q: pool and datacenter are required", g.Name))
		case g.Size.CPU <= 0 || g.Size.Memory <= 0:
			mErr = multierror.Append(mErr, fmt.Errorf("advice %q: cpu and memory must be greater than 0", g.Name))
		case g.Size.Disk < 0:
			mErr = multierror.Append(mErr, fmt.Errorf("advice %q: disk must not be negative", g.Name))
		case seen[[2]string{g.Pool, g.Datacenter}]:
			mErr = multierror.Append(mErr, fmt.Errorf("advice %q: pool %q in datacenter %q is listed twice", g.Name, g.Pool, g.Datacenter))
		}
		seen[[2]string{g.Pool, g.Datacenter}] = true
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
		if !isBuiltinAPMDriver(a.Driver) && c.PluginDir == "" {
			mErr = multierror.Append(mErr, fmt.Errorf(
				"apm %q: driver %q is not a built-in plugin and plugin_dir is not set", a.Name, a.Driver))
		}
	}

	names = map[string]bool{}
	for _, s := range c.Strategies {
		if s.Name == "" {
			mErr = multierror.Append(mErr, fmt.Errorf("strategy block must have a name"))
			continue
		}
		if names[s.Name] {
			mErr = multierror.Append(mErr, fmt.Errorf("duplicate strategy %q", s.Name))
		}
		names[s.Name] = true
		if !isBuiltinStrategyDriver(s.Driver) && c.PluginDir == "" {
			mErr = multierror.Append(mErr, fmt.Errorf(
				"strategy %q: driver %q is not a built-in plugin and plugin_dir is not set", s.Name, s.Driver))
		}
	}

	return mErr.ErrorOrNil()
}

// builtinStrategies are the strategies that are always available.
var builtinStrategies = []string{
	plugins.InternalStrategyTargetValue,
	plugins.InternalStrategyThreshold,
	plugins.InternalStrategyFixedValue,
	plugins.InternalStrategyPassThrough,
}

func isBuiltinAPMDriver(driver string) bool {
	return driver == plugins.InternalAPMNomad || driver == plugins.InternalAPMPrometheus
}

func isBuiltinStrategyDriver(driver string) bool {
	return slices.Contains(builtinStrategies, driver)
}

// KnownSources returns the names of the APMs a check may use as source: the
// built-in nomad-apm and norca, and the configured (built-in or external)
// APMs.
func (c *Config) KnownSources() []string {
	names := []string{plugins.InternalAPMNomad, plugins.InternalAPMNorca}
	for _, a := range c.APMs {
		if !slices.Contains(names, a.Name) {
			names = append(names, a.Name)
		}
	}
	return names
}

// KnownStrategies returns the names of the strategies a check may use: the
// built-in strategies and the configured (built-in or external) ones.
func (c *Config) KnownStrategies() []string {
	names := slices.Clone(builtinStrategies)
	for _, s := range c.Strategies {
		if !slices.Contains(names, s.Name) {
			names = append(names, s.Name)
		}
	}
	return names
}

// nomadAPMNames returns the names of the APMs backed by the nomad-apm
// driver, including the always available nomad-apm.
func (c *Config) nomadAPMNames() []string {
	names := []string{plugins.InternalAPMNomad}
	for _, a := range c.APMs {
		if a.Driver == plugins.InternalAPMNomad && !slices.Contains(names, a.Name) {
			names = append(names, a.Name)
		}
	}
	return names
}

// StrategyDriver returns the driver of the named strategy, or "" if the
// strategy is unknown.
func (c *Config) StrategyDriver(name string) string {
	for _, s := range c.Strategies {
		if s.Name == name {
			return s.Driver
		}
	}
	if isBuiltinStrategyDriver(name) {
		return name
	}
	return ""
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
