// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package external

import (
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
)

const (
	// DefaultRestartBackoff is the wait before the first restart of a plugin
	// that crashed or failed to start; it doubles on every further failure
	// up to DefaultMaxRestartBackoff.
	DefaultRestartBackoff    = time.Second
	DefaultMaxRestartBackoff = 2 * time.Minute

	// stableAfter is how long a plugin must run before its earlier failures
	// are forgotten.
	stableAfter = 5 * time.Minute
)

// ErrShutdown is returned by the plugins of a loader that was shut down.
var ErrShutdown = errors.New("plugin host is shut down")

// Loader launches the external plugins of the built-in autoscaler from a
// plugin directory and supervises them. It implements the autoscaler's
// ExternalPluginLoader.
//
// Every plugin runs in its own process. A plugin that crashes, hangs or
// fails to start only makes the calls to it fail, which fails the checks
// that use it; the plugin is restarted on its next use, with exponential
// backoff between attempts.
type Loader struct {
	logger hclog.Logger
	dir    string

	// RestartBackoff and MaxRestartBackoff configure the restart backoff.
	// They can be changed before the first plugin is loaded.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration

	// now returns the current time; replaced in tests.
	now func() time.Time

	l         sync.Mutex
	instances []*instance
	shutdown  bool
}

// NewLoader returns a loader for the plugin binaries in dir.
func NewLoader(logger hclog.Logger, dir string) *Loader {
	l := &Loader{
		logger:            logger.Named("external_plugins"),
		dir:               dir,
		RestartBackoff:    DefaultRestartBackoff,
		MaxRestartBackoff: DefaultMaxRestartBackoff,
		now:               time.Now,
	}
	if names, err := Discover(dir); err != nil {
		l.logger.Warn("failed to list plugin directory", "error", err)
	} else {
		l.logger.Debug("discovered plugin binaries", "plugin_dir", dir, "plugins", names)
	}
	return l
}

// APM returns the APM plugin named name, served by the binary driver of the
// plugin directory and configured with config. The plugin is started right
// away; if that fails the error is logged and the start is retried on use.
func (l *Loader) APM(name, driver string, args []string, config map[string]string) (plugins.APM, error) {
	inst, err := l.load(name, driver, sdk.PluginTypeAPM, args, config)
	if err != nil {
		return nil, err
	}
	return &apmInstance{inst}, nil
}

// Strategy returns the strategy plugin named name, served by the binary
// driver of the plugin directory.
func (l *Loader) Strategy(name, driver string, args []string, config map[string]string) (plugins.Strategy, error) {
	inst, err := l.load(name, driver, sdk.PluginTypeStrategy, args, config)
	if err != nil {
		return nil, err
	}
	return &strategyInstance{inst}, nil
}

// Shutdown stops all plugin processes. The plugins of the loader return
// ErrShutdown afterwards.
func (l *Loader) Shutdown() {
	l.l.Lock()
	l.shutdown = true
	instances := l.instances
	l.instances = nil
	l.l.Unlock()

	for _, inst := range instances {
		inst.close()
	}
}

func (l *Loader) load(name, driver, pluginType string, args []string, config map[string]string) (*instance, error) {
	if driver == "" {
		driver = name
	}
	if _, err := resolve(l.dir, driver); err != nil {
		// The binary may be installed later; it is looked up again on every
		// start attempt.
		l.logger.Warn("external plugin binary not found, it will be looked up again when used",
			"plugin", name, "error", err)
	}

	inst := &instance{
		loader:     l,
		name:       name,
		driver:     driver,
		pluginType: pluginType,
		args:       slices.Clone(args),
		config:     maps.Clone(config),
		logger:     l.logger.With("plugin", name, "driver", driver, "type", pluginType),
	}
	if inst.config == nil {
		inst.config = map[string]string{}
	}

	l.l.Lock()
	if l.shutdown {
		l.l.Unlock()
		return nil, ErrShutdown
	}
	l.instances = append(l.instances, inst)
	l.l.Unlock()

	if _, err := inst.get(); err != nil {
		inst.logger.Error("failed to start external plugin, it will be restarted when used", "error", err)
	} else {
		inst.logger.Info("started external plugin")
	}
	return inst, nil
}

// instance is a supervised external plugin process.
type instance struct {
	loader     *Loader
	name       string
	driver     string
	pluginType string
	args       []string
	logger     hclog.Logger

	l        sync.Mutex
	config   map[string]string
	client   *plugin.Client
	raw      plugins.Base
	started  time.Time
	failures int
	retryAt  time.Time
	closed   bool
}

// get returns the running plugin, restarting it if it has exited and the
// restart backoff allows it.
func (i *instance) get() (plugins.Base, error) {
	i.l.Lock()
	defer i.l.Unlock()

	if i.closed {
		return nil, ErrShutdown
	}

	now := i.loader.now()
	if i.client != nil {
		if !i.client.Exited() {
			return i.raw, nil
		}
		i.logger.Warn("external plugin exited unexpectedly")
		metrics.IncrCounterWithLabels([]string{"nomad", "autoscaler", "plugin", "exited"}, 1,
			[]metrics.Label{{Name: "plugin_name", Value: i.name}})
		i.client.Kill()
		i.client, i.raw = nil, nil
		i.failedLocked(now, now.Sub(i.started) >= stableAfter)
	}

	if now.Before(i.retryAt) {
		return nil, fmt.Errorf("external plugin %q is not running, next start attempt in %s",
			i.name, i.retryAt.Sub(now).Round(time.Millisecond))
	}

	client, raw, err := i.launch()
	if err != nil {
		i.failedLocked(now, false)
		return nil, err
	}
	if i.failures > 0 {
		i.logger.Info("restarted external plugin")
	}
	i.client, i.raw, i.started = client, raw, now
	return raw, nil
}

// failedLocked records a failure and schedules the next start attempt. The
// lock must be held.
func (i *instance) failedLocked(now time.Time, wasStable bool) {
	if wasStable {
		i.failures = 0
	}
	i.failures++

	backoff := i.loader.RestartBackoff
	for n := 1; n < i.failures && backoff < i.loader.MaxRestartBackoff; n++ {
		backoff *= 2
	}
	if backoff > i.loader.MaxRestartBackoff {
		backoff = i.loader.MaxRestartBackoff
	}
	i.retryAt = now.Add(backoff)
}

// launch starts the plugin process, dispenses the plugin, checks that it is
// the expected plugin and configures it.
func (i *instance) launch() (*plugin.Client, plugins.Base, error) {
	path, err := resolve(i.loader.dir, i.driver)
	if err != nil {
		return nil, nil, err
	}

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          pluginMap(i.pluginType),
		Cmd:              exec.Command(path, i.args...),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           i.logger,
	})

	fail := func(err error) (*plugin.Client, plugins.Base, error) {
		client.Kill()
		return nil, nil, err
	}

	rpcClient, err := client.Client()
	if err != nil {
		return fail(fmt.Errorf("failed to start plugin %q: %w", i.name, err))
	}

	raw, err := rpcClient.Dispense(i.pluginType)
	if err != nil {
		return fail(fmt.Errorf("failed to dispense plugin %q: %w", i.name, err))
	}

	base, ok := raw.(plugins.Base)
	if !ok {
		return fail(fmt.Errorf("plugin %q does not implement the base plugin", i.name))
	}

	info, err := base.PluginInfo()
	if err != nil {
		return fail(fmt.Errorf("failed to call PluginInfo on %q: %w", i.name, err))
	}

	// The executed plugin must identify itself as the configured driver and
	// type, otherwise it is unable to fulfill its role.
	if info.Name != i.driver || info.PluginType != i.pluginType {
		return fail(fmt.Errorf("plugin %q identifies itself as %q of type %q, expected %q of type %q",
			i.name, info.Name, info.PluginType, i.driver, i.pluginType))
	}

	if err := base.SetConfig(i.config); err != nil {
		return fail(fmt.Errorf("failed to set config on plugin %q: %w", i.name, err))
	}

	return client, base, nil
}

// setConfig stores the configuration and applies it to the running plugin.
func (i *instance) setConfig(config map[string]string) error {
	i.l.Lock()
	i.config = maps.Clone(config)
	if i.config == nil {
		i.config = map[string]string{}
	}
	i.l.Unlock()

	raw, err := i.get()
	if err != nil {
		return err
	}
	return raw.SetConfig(config)
}

// close kills the plugin process.
func (i *instance) close() {
	i.l.Lock()
	defer i.l.Unlock()

	i.closed = true
	if i.client != nil {
		i.client.Kill()
		i.client, i.raw = nil, nil
	}
}

// call runs fn against the running plugin. A panic while handling the
// plugin's response is turned into an error, so a misbehaving plugin cannot
// take down the server.
func (i *instance) call(fn func(raw plugins.Base) error) (err error) {
	raw, err := i.get()
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("external plugin %q: %v", i.name, r)
		}
	}()
	return fn(raw)
}

func (i *instance) PluginInfo() (*plugins.PluginInfo, error) {
	var info *plugins.PluginInfo
	err := i.call(func(raw plugins.Base) error {
		var err error
		info, err = raw.PluginInfo()
		return err
	})
	return info, err
}

func (i *instance) SetConfig(config map[string]string) error {
	return i.setConfig(config)
}

// apmInstance is a supervised external APM plugin.
type apmInstance struct {
	*instance
}

func (a *apmInstance) Query(q string, r sdk.TimeRange) (sdk.TimestampedMetrics, error) {
	var out sdk.TimestampedMetrics
	err := a.call(func(raw plugins.Base) error {
		var err error
		out, err = raw.(plugins.APM).Query(q, r)
		return err
	})
	return out, err
}

func (a *apmInstance) QueryMultiple(q string, r sdk.TimeRange) ([]sdk.TimestampedMetrics, error) {
	var out []sdk.TimestampedMetrics
	err := a.call(func(raw plugins.Base) error {
		var err error
		out, err = raw.(plugins.APM).QueryMultiple(q, r)
		return err
	})
	return out, err
}

// strategyInstance is a supervised external strategy plugin.
type strategyInstance struct {
	*instance
}

func (s *strategyInstance) Run(eval *sdk.ScalingCheckEvaluation, count int64) (*sdk.ScalingCheckEvaluation, error) {
	var out *sdk.ScalingCheckEvaluation
	err := s.call(func(raw plugins.Base) error {
		var err error
		out, err = raw.(plugins.Strategy).Run(eval, count)
		return err
	})
	return out, err
}
