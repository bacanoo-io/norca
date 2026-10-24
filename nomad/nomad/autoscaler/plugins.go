// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/apm/nomadapm"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/apm/prometheus"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/fixedvalue"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/passthrough"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/targetvalue"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/threshold"
)

// ExternalPluginLoader is the seam for plugins that run outside the server
// process (the external autoscaler's go-plugin based plugins loaded from
// plugin_dir). It is not implemented yet: loading them needs the go-plugin
// and gRPC versions used by the autoscaler plugin SDK. When it exists, the
// plugin host asks it for every configured plugin whose driver is not
// built-in.
type ExternalPluginLoader interface {
	// APM returns a running APM plugin instance for the driver, configured
	// with config.
	APM(name, driver string, config map[string]string) (plugins.APM, error)

	// Strategy returns a running strategy plugin instance for the driver.
	Strategy(name, driver string, config map[string]string) (plugins.Strategy, error)

	// Shutdown stops all plugin processes.
	Shutdown()
}

// pluginHost owns the plugin instances used by the policy evaluations.
type pluginHost struct {
	strategies map[string]plugins.StrategyRunner
	apms       map[string]plugins.Looker

	// nomadAPMs are the names of the APMs backed by the nomad-apm driver;
	// their short queries are canonicalized for the policy target.
	nomadAPMs []string

	external ExternalPluginLoader
}

// newPluginHost instantiates the built-in plugins and the configured APMs.
func newPluginHost(logger hclog.Logger, cfg *Config, deps *Deps) (*pluginHost, error) {
	h := &pluginHost{
		strategies: map[string]plugins.StrategyRunner{},
		apms:       map[string]plugins.Looker{},
		external:   deps.ExternalPlugins,
	}

	strategyLogger := logger.Named("strategy")
	h.strategies[plugins.InternalStrategyTargetValue] = targetvalue.NewTargetValuePlugin(strategyLogger.Named(plugins.InternalStrategyTargetValue))
	h.strategies[plugins.InternalStrategyThreshold] = threshold.NewThresholdPlugin(strategyLogger.Named(plugins.InternalStrategyThreshold))
	h.strategies[plugins.InternalStrategyFixedValue] = fixedvalue.NewFixedValuePlugin(strategyLogger.Named(plugins.InternalStrategyFixedValue))
	h.strategies[plugins.InternalStrategyPassThrough] = passthrough.NewPassThroughPlugin(strategyLogger.Named(plugins.InternalStrategyPassThrough))

	apmLogger := logger.Named("apm")
	newNomadAPM := func(name string) plugins.APM {
		return nomadapm.NewNomadPlugin(apmLogger.Named(name), deps.State, deps.AllocStats)
	}

	// The nomad-apm is always available, like in the external autoscaler.
	h.apms[plugins.InternalAPMNomad] = newNomadAPM(plugins.InternalAPMNomad)
	h.nomadAPMs = []string{plugins.InternalAPMNomad}

	for _, pc := range cfg.APMs {
		var p plugins.APM
		switch pc.Driver {
		case plugins.InternalAPMNomad:
			p = newNomadAPM(pc.Name)
			if pc.Name != plugins.InternalAPMNomad {
				h.nomadAPMs = append(h.nomadAPMs, pc.Name)
			}
		case plugins.InternalAPMPrometheus:
			p = prometheus.NewPrometheusPlugin(apmLogger.Named(pc.Name))
		default:
			if h.external == nil {
				return nil, fmt.Errorf("apm %q: driver %q is not a built-in plugin and external plugins are not supported",
					pc.Name, pc.Driver)
			}
			ext, err := h.external.APM(pc.Name, pc.Driver, pc.Config)
			if err != nil {
				return nil, fmt.Errorf("apm %q: %w", pc.Name, err)
			}
			h.apms[pc.Name] = ext
			continue
		}

		config := pc.Config
		if config == nil {
			config = map[string]string{}
		}
		if err := p.SetConfig(config); err != nil {
			return nil, fmt.Errorf("apm %q: failed to configure: %w", pc.Name, err)
		}
		h.apms[pc.Name] = p
	}

	return h, nil
}

// getStrategyRunner returns the strategy plugin with the given name.
func (h *pluginHost) getStrategyRunner(name string) (plugins.StrategyRunner, error) {
	s, ok := h.strategies[name]
	if !ok {
		return nil, fmt.Errorf("strategy %q not found", name)
	}
	return s, nil
}

// getAPMLooker returns the APM plugin with the given name.
func (h *pluginHost) getAPMLooker(name string) (plugins.Looker, error) {
	a, ok := h.apms[name]
	if !ok {
		return nil, fmt.Errorf("apm %q not found", name)
	}
	return a, nil
}

// shutdown releases the plugin resources.
func (h *pluginHost) shutdown() {
	if h.external != nil {
		h.external.Shutdown()
	}
}
