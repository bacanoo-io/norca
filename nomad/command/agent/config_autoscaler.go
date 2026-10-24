// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"slices"
	"time"

	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/mitchellh/copystructure"
)

// AutoscalerConfig is the server's autoscaler block. It configures the
// built-in autoscaler that runs on the leader.
type AutoscalerConfig struct {
	// Enabled starts the built-in autoscaler on the leader. It can be
	// changed with a configuration reload.
	Enabled *bool `hcl:"enabled"`

	// DefaultEngine is the engine of scaling policies that do not set one:
	// "external" (default) or "builtin".
	DefaultEngine string `hcl:"default_engine"`

	// MinEvalInterval is the lower bound of a policy's evaluation_interval.
	MinEvalInterval string `hcl:"min_eval_interval"`

	// DefaultEvaluationInterval and DefaultCooldown apply to policies that
	// do not set evaluation_interval or cooldown.
	DefaultEvaluationInterval string `hcl:"default_evaluation_interval"`
	DefaultCooldown           string `hcl:"default_cooldown"`

	// PluginDir is the directory of external autoscaler plugins. External
	// plugins are not supported yet.
	PluginDir string `hcl:"plugin_dir"`

	// EvalWorkers is the number of policy evaluations that may run in
	// parallel.
	EvalWorkers int `hcl:"eval_workers"`

	// RateLimit and RateLimitBurst configure the cluster-wide token bucket on
	// scaling actions.
	RateLimit      float64 `hcl:"rate_limit"`
	RateLimitBurst int     `hcl:"rate_limit_burst"`

	// APMs configures named APM instances, such as a Prometheus server.
	APMs []*AutoscalerPluginConfig `hcl:"apm"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// AutoscalerPluginConfig configures an autoscaler plugin instance.
type AutoscalerPluginConfig struct {
	Name   string                 `hcl:",key"`
	Driver string                 `hcl:"driver"`
	Config map[string]interface{} `hcl:"config"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a deep copy of the plugin configuration.
func (p *AutoscalerPluginConfig) Copy() *AutoscalerPluginConfig {
	if p == nil {
		return nil
	}
	np := *p
	if p.Config != nil {
		c, err := copystructure.Copy(p.Config)
		if err != nil {
			panic(err.Error())
		}
		np.Config = c.(map[string]interface{})
	}
	np.ExtraKeysHCL = slices.Clone(p.ExtraKeysHCL)
	return &np
}

// Copy returns a deep copy of the autoscaler configuration.
func (a *AutoscalerConfig) Copy() *AutoscalerConfig {
	if a == nil {
		return nil
	}
	na := *a
	na.Enabled = pointer.Copy(a.Enabled)
	na.APMs = nil
	for _, p := range a.APMs {
		na.APMs = append(na.APMs, p.Copy())
	}
	na.ExtraKeysHCL = slices.Clone(a.ExtraKeysHCL)
	return &na
}

// Merge merges b into a copy of a. APM blocks of b replace those of a with
// the same name.
func (a *AutoscalerConfig) Merge(b *AutoscalerConfig) *AutoscalerConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b == nil {
		return result
	}

	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.DefaultEngine != "" {
		result.DefaultEngine = b.DefaultEngine
	}
	if b.MinEvalInterval != "" {
		result.MinEvalInterval = b.MinEvalInterval
	}
	if b.DefaultEvaluationInterval != "" {
		result.DefaultEvaluationInterval = b.DefaultEvaluationInterval
	}
	if b.DefaultCooldown != "" {
		result.DefaultCooldown = b.DefaultCooldown
	}
	if b.PluginDir != "" {
		result.PluginDir = b.PluginDir
	}
	if b.EvalWorkers != 0 {
		result.EvalWorkers = b.EvalWorkers
	}
	if b.RateLimit != 0 {
		result.RateLimit = b.RateLimit
	}
	if b.RateLimitBurst != 0 {
		result.RateLimitBurst = b.RateLimitBurst
	}
	for _, bp := range b.APMs {
		replaced := false
		for i, rp := range result.APMs {
			if rp.Name == bp.Name {
				result.APMs[i] = bp.Copy()
				replaced = true
				break
			}
		}
		if !replaced {
			result.APMs = append(result.APMs, bp.Copy())
		}
	}
	return result
}

// toServerConfig converts the agent configuration into the server's
// autoscaler configuration, applying defaults and validating it.
func (a *AutoscalerConfig) toServerConfig() (*autoscaler.Config, error) {
	cfg := autoscaler.DefaultConfig()
	if a == nil {
		return cfg, nil
	}

	if a.Enabled != nil {
		cfg.Enabled = *a.Enabled
	}
	if a.DefaultEngine != "" {
		cfg.DefaultEngine = a.DefaultEngine
	}

	durations := []struct {
		name  string
		value string
		field *time.Duration
	}{
		{"min_eval_interval", a.MinEvalInterval, &cfg.MinEvalInterval},
		{"default_evaluation_interval", a.DefaultEvaluationInterval, &cfg.DefaultEvaluationInterval},
		{"default_cooldown", a.DefaultCooldown, &cfg.DefaultCooldown},
	}
	for _, d := range durations {
		if d.value == "" {
			continue
		}
		v, err := time.ParseDuration(d.value)
		if err != nil {
			return nil, fmt.Errorf("autoscaler.%s can't parse time duration %s", d.name, d.value)
		}
		*d.field = v
	}

	cfg.PluginDir = a.PluginDir
	if a.EvalWorkers != 0 {
		cfg.EvalWorkers = a.EvalWorkers
	}
	if a.RateLimit != 0 {
		cfg.RateLimit = a.RateLimit
	}
	if a.RateLimitBurst != 0 {
		cfg.RateLimitBurst = a.RateLimitBurst
	}

	for _, p := range a.APMs {
		pc := &autoscaler.PluginConfig{
			Name:   p.Name,
			Driver: p.Driver,
			Config: make(map[string]string, len(p.Config)),
		}
		if pc.Driver == "" {
			pc.Driver = p.Name
		}
		for k, v := range p.Config {
			pc.Config[k] = fmt.Sprintf("%v", v)
		}
		cfg.APMs = append(cfg.APMs, pc)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid autoscaler configuration: %w", err)
	}
	return cfg, nil
}
