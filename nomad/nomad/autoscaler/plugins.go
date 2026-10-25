// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/apm/nomadapm"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/apm/prometheus"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/external"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/fixedvalue"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/passthrough"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/targetvalue"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/strategy/threshold"
)

// ExternalPluginLoader loads the plugins that run outside the server
// process: APM and strategy plugins built with the Nomad Autoscaler plugin
// SDK, launched with go-plugin from plugin_dir. The plugin host asks it for
// every configured plugin whose driver is not built-in.
//
// The returned plugins must isolate failures: a crashed or hung plugin
// process only makes the calls to it fail.
type ExternalPluginLoader interface {
	// APM returns a running APM plugin instance for the driver (the binary
	// name in plugin_dir), started with args and configured with config.
	APM(name, driver string, args []string, config map[string]string) (plugins.APM, error)

	// Strategy returns a running strategy plugin instance for the driver.
	Strategy(name, driver string, args []string, config map[string]string) (plugins.Strategy, error)

	// Shutdown stops all plugin processes.
	Shutdown()
}

// defaultExternalPlugins creates the go-plugin based loader.
func defaultExternalPlugins(logger hclog.Logger, dir string) ExternalPluginLoader {
	return external.NewLoader(logger, dir)
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

// newPluginHost instantiates the built-in plugins and the configured APMs
// and strategies. external may be nil if no plugin_dir is configured.
func newPluginHost(logger hclog.Logger, cfg *Config, deps *Deps, external ExternalPluginLoader) (*pluginHost, error) {
	h := &pluginHost{
		strategies: map[string]plugins.StrategyRunner{},
		apms:       map[string]plugins.Looker{},
		external:   external,
	}

	strategyLogger := logger.Named("strategy")
	newBuiltinStrategy := func(driver, name string) plugins.Strategy {
		l := strategyLogger.Named(name)
		switch driver {
		case plugins.InternalStrategyTargetValue:
			return targetvalue.NewTargetValuePlugin(l)
		case plugins.InternalStrategyThreshold:
			return threshold.NewThresholdPlugin(l)
		case plugins.InternalStrategyFixedValue:
			return fixedvalue.NewFixedValuePlugin(l)
		case plugins.InternalStrategyPassThrough:
			return passthrough.NewPassThroughPlugin(l)
		}
		return nil
	}
	for _, name := range builtinStrategies {
		h.strategies[name] = newBuiltinStrategy(name, name)
	}

	for _, sc := range cfg.Strategies {
		if s := newBuiltinStrategy(sc.Driver, sc.Name); s != nil {
			if err := s.SetConfig(nonNilConfig(sc.Config)); err != nil {
				return nil, fmt.Errorf("strategy %q: failed to configure: %w", sc.Name, err)
			}
			h.strategies[sc.Name] = s
			continue
		}
		if h.external == nil {
			return nil, fmt.Errorf("strategy %q: driver %q is not a built-in plugin and plugin_dir is not set",
				sc.Name, sc.Driver)
		}
		ext, err := h.external.Strategy(sc.Name, sc.Driver, sc.Args, sc.Config)
		if err != nil {
			return nil, fmt.Errorf("strategy %q: %w", sc.Name, err)
		}
		h.strategies[sc.Name] = ext
	}

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
				return nil, fmt.Errorf("apm %q: driver %q is not a built-in plugin and plugin_dir is not set",
					pc.Name, pc.Driver)
			}
			// External plugins are configured by the loader, on every
			// (re)start of their process.
			ext, err := h.external.APM(pc.Name, pc.Driver, pc.Args, pc.Config)
			if err != nil {
				return nil, fmt.Errorf("apm %q: %w", pc.Name, err)
			}
			h.apms[pc.Name] = ext
			continue
		}

		if err := p.SetConfig(nonNilConfig(pc.Config)); err != nil {
			return nil, fmt.Errorf("apm %q: failed to configure: %w", pc.Name, err)
		}
		h.apms[pc.Name] = p
	}

	return h, nil
}

func nonNilConfig(c map[string]string) map[string]string {
	if c == nil {
		return map[string]string{}
	}
	return c
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
