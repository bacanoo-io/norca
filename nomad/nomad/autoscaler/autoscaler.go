// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package autoscaler is the built-in autoscaler. It runs on the leader server,
// evaluates the scaling policies of the "builtin" engine with the strategies
// and APMs of the Nomad Autoscaler (ported to run in-process), and executes
// its decisions through the existing Job.Scale path.
package autoscaler

import (
	"context"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins/apm/nomadapm"
	"github.com/hashicorp/nomad/nomad/state"
)

// versionCheckInterval is how often the autoscaler checks whether all servers
// run a version that supports it.
var versionCheckInterval = 10 * time.Second

// Deps are the server facilities the autoscaler uses.
type Deps struct {
	// State returns the current state store.
	State func() *state.StateStore

	// Scaler executes scaling actions through Job.Scale.
	Scaler JobScaler

	// ObjectScaler executes scaling actions on custom objects through their
	// scale subresource. Optional; without it, policies with a custom object
	// target are invalid.
	ObjectScaler ObjectScaler

	// AllocStats returns the resource usage of an allocation. Used by the
	// plugin host's nomad-apm instance, which the evaluations no longer
	// query: checks of the nomad-apm source are served from the usage pushed
	// by clients. Optional.
	AllocStats nomadapm.AllocStatsFn

	// UsageHistory reads the usage of a task group over a range from the
	// metrics store: the mean of the interval averages, the largest
	// maximum and the number of intervals. metric is the store's name
	// (norca.group.cpu, cpu_mhz, memory, memory_mb). Optional; with it the
	// built-in metrics serve query windows longer than metrics_window, and
	// the time after a leader change before the window is full.
	UsageHistory func(namespace, job, group, metric string, from, to time.Time) (avg, max float64, samples int, err error)

	// PublishEvent publishes the events of the Scaling topic. Optional.
	PublishEvent EventPublisher

	// ServersReady reports whether all servers of the region support the
	// built-in autoscaler (the shared Norca version gate).
	ServersReady func() bool

	// ExternalPlugins creates the loader of the plugins that are not
	// built-in, for the plugin directory of the configuration. Optional,
	// defaults to the go-plugin based loader of the external package. A new
	// loader is created for every run and shut down when the run ends.
	ExternalPlugins func(logger hclog.Logger, dir string) ExternalPluginLoader

	// Now returns the current time. Optional, defaults to time.Now. Tests
	// use it to inject a fake clock.
	Now func() time.Time
}

// Autoscaler is the built-in autoscaler. It is created with the server and
// runs while the server is the leader and the autoscaler is enabled.
type Autoscaler struct {
	logger hclog.Logger
	deps   *Deps
	now    func() time.Time

	// l protects the fields below.
	l      sync.Mutex
	config *Config
	leader bool

	// cancel stops the current run; nil when not running.
	cancel context.CancelFunc
	doneCh chan struct{}

	// usage keeps the task group usage reported by clients while the
	// autoscaler runs.
	usage *usageCollector
}

// New returns a new, stopped autoscaler.
func New(logger hclog.Logger, config *Config, deps *Deps) *Autoscaler {
	if config == nil {
		config = DefaultConfig()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Autoscaler{
		logger: logger.Named("autoscaler"),
		deps:   deps,
		now:    now,
		config: config.Copy(),
		usage:  newUsageCollector(),
	}
}

// SetLeader starts or stops the autoscaler on leadership changes.
func (a *Autoscaler) SetLeader(leader bool) {
	a.l.Lock()
	defer a.l.Unlock()

	a.leader = leader
	a.reconcileLocked(false)
}

// SeedUsage fills the usage window of a new leader with the usage of before
// it became leader, from the metrics store, and returns the number of seeds
// used. It does nothing while the autoscaler does not run on this server.
func (a *Autoscaler) SeedUsage(seeds []UsageSeed) int {
	return a.usage.seed(a.now(), seeds)
}

// MetricsWindow returns the window of the built-in metrics.
func (a *Autoscaler) MetricsWindow() time.Duration {
	a.l.Lock()
	defer a.l.Unlock()
	if a.config.MetricsWindow <= 0 {
		return DefaultMetricsWindow
	}
	return a.config.MetricsWindow
}

// SetConfig applies a reloaded configuration. A running autoscaler is
// restarted if the configuration changed, and stopped if it was disabled.
func (a *Autoscaler) SetConfig(config *Config) {
	if config == nil {
		return
	}

	a.l.Lock()
	defer a.l.Unlock()

	changed := !configEqual(a.config, config)
	a.config = config.Copy()
	a.reconcileLocked(changed)
}

// Config returns a copy of the current configuration.
func (a *Autoscaler) Config() *Config {
	a.l.Lock()
	defer a.l.Unlock()
	return a.config.Copy()
}

// DefaultEngine returns the engine of policies that do not set one.
func (a *Autoscaler) DefaultEngine() string {
	a.l.Lock()
	defer a.l.Unlock()
	return a.config.DefaultEngine
}

// Running reports whether the autoscaler is running.
func (a *Autoscaler) Running() bool {
	a.l.Lock()
	defer a.l.Unlock()
	return a.cancel != nil
}

// Stop stops the autoscaler and waits for it to exit.
func (a *Autoscaler) Stop() {
	a.l.Lock()
	doneCh := a.doneCh
	a.leader = false
	a.reconcileLocked(false)
	a.l.Unlock()

	if doneCh != nil {
		<-doneCh
	}
}

// reconcileLocked starts or stops the run to match the desired state. The
// lock must be held.
func (a *Autoscaler) reconcileLocked(restart bool) {
	shouldRun := a.leader && a.config.Enabled

	// The usage collector starts empty on every leader change.
	a.usage.setActive(shouldRun, a.config.MetricsWindow, a.now())

	if a.cancel != nil && (!shouldRun || restart) {
		a.logger.Info("stopping built-in autoscaler")
		a.cancel()
		a.cancel = nil
		a.doneCh = nil
	}

	if shouldRun && a.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		doneCh := make(chan struct{})
		a.cancel = cancel
		a.doneCh = doneCh
		go a.run(ctx, a.config.Copy(), doneCh)
	}
}

// run is one run of the autoscaler. It blocks until ctx is canceled.
func (a *Autoscaler) run(ctx context.Context, cfg *Config, doneCh chan struct{}) {
	defer close(doneCh)

	// The built-in engine starts only when all servers of the region run a
	// version that supports it.
	for a.deps.ServersReady != nil && !a.deps.ServersReady() {
		a.logger.Debug("waiting for all servers to support the built-in autoscaler")
		select {
		case <-ctx.Done():
			return
		case <-time.After(versionCheckInterval):
		}
	}

	// External plugin processes live as long as this run: they are stopped
	// when leadership is lost, the autoscaler is disabled or reconfigured.
	var external ExternalPluginLoader
	if cfg.PluginDir != "" {
		newLoader := a.deps.ExternalPlugins
		if newLoader == nil {
			newLoader = defaultExternalPlugins
		}
		external = newLoader(a.logger, cfg.PluginDir)
	}

	host, err := newPluginHost(a.logger, cfg, a.deps, external)
	if err != nil {
		if external != nil {
			external.Shutdown()
		}
		a.logger.Error("failed to set up autoscaler plugins, the built-in autoscaler will not run", "error", err)
		return
	}
	defer host.shutdown()

	a.logger.Info("starting built-in autoscaler", "default_engine", cfg.DefaultEngine,
		"eval_workers", cfg.EvalWorkers)
	r := newRunner(a.logger, cfg, a.deps, a.now, host)
	r.usage = a.usage
	r.run(ctx)
}

// configEqual reports whether two configurations are the same.
func configEqual(a, b *Config) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Enabled != b.Enabled ||
		a.DefaultEngine != b.DefaultEngine ||
		a.MinEvalInterval != b.MinEvalInterval ||
		a.MinCooldown != b.MinCooldown ||
		a.DefaultEvaluationInterval != b.DefaultEvaluationInterval ||
		a.DefaultCooldown != b.DefaultCooldown ||
		a.PluginDir != b.PluginDir ||
		a.EvalWorkers != b.EvalWorkers ||
		a.RateLimit != b.RateLimit ||
		a.RateLimitBurst != b.RateLimitBurst ||
		a.OscillationWindow != b.OscillationWindow ||
		a.OscillationThreshold != b.OscillationThreshold ||
		a.OscillationSuspend != b.OscillationSuspend ||
		len(a.APMs) != len(b.APMs) ||
		len(a.Strategies) != len(b.Strategies) {
		return false
	}
	for i := range a.APMs {
		if !a.APMs[i].equal(b.APMs[i]) {
			return false
		}
	}
	for i := range a.Strategies {
		if !a.Strategies[i].equal(b.Strategies[i]) {
			return false
		}
	}
	return true
}
