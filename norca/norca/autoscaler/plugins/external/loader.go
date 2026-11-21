// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package external

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	hclog "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	// beforeLaunch, set by tests, is called when a start of a process
	// begins, without the lock of the instance.
	beforeLaunch func()

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
	// The name the plugin identifies itself with (review finding ASP-12).
	driver = driverName(driver)
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

	// starting is closed when the start in progress ends; nil when none is
	// (Norca, review finding ASP-8: the start ran under the lock, and every
	// call, setConfig and close waited for it, up to about three minutes).
	starting chan struct{}

	// timeouts counts the calls in a row that ran out of CallTimeout on the
	// running process (Norca, hungAfter).
	timeouts int
}

// hungAfter is the number of calls in a row that run out of CallTimeout after
// which a plugin process that has not exited counts as hung: it is stopped and
// started again like one that exited (review finding ASP-7). Before, only an
// exited process was restarted, and a deadlocked one held every call for the
// whole timeout, for good.
const hungAfter = 3

// get returns the running plugin, restarting it if it has exited and the
// restart backoff allows it. The process is started without the lock held,
// once for all the callers that need it meanwhile: they wait for that start.
func (i *instance) get() (plugins.Base, error) {
	i.l.Lock()
	for i.starting != nil && !i.closed {
		starting := i.starting
		i.l.Unlock()
		<-starting
		i.l.Lock()
	}
	if i.closed {
		i.l.Unlock()
		return nil, ErrShutdown
	}

	now := i.loader.now()
	if i.client != nil {
		if !i.client.Exited() {
			raw := i.raw
			i.l.Unlock()
			return raw, nil
		}
		i.logger.Warn("external plugin exited unexpectedly")
		metrics.IncrCounterWithLabels([]string{"nomad", "autoscaler", "plugin", "exited"}, 1,
			[]metrics.Label{{Name: "plugin_name", Value: i.name}})
		i.client.Kill()
		i.client, i.raw = nil, nil
		i.failedLocked(now, now.Sub(i.started) >= stableAfter)
	}

	if now.Before(i.retryAt) {
		i.l.Unlock()
		return nil, fmt.Errorf("external plugin %q is not running, next start attempt in %s",
			i.name, i.retryAt.Sub(now).Round(time.Millisecond))
	}

	starting := make(chan struct{})
	i.starting = starting
	config := maps.Clone(i.config)
	i.l.Unlock()

	if hook := i.loader.beforeLaunch; hook != nil {
		hook()
	}
	client, raw, err := i.launch(config)

	i.l.Lock()
	defer i.l.Unlock()
	i.starting = nil
	close(starting)
	if i.closed {
		// Closed while it started: the process does not outlive the loader.
		if client != nil {
			client.Kill()
		}
		return nil, ErrShutdown
	}
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
// the expected plugin and configures it with config. It runs without the lock.
func (i *instance) launch(config map[string]string) (*plugin.Client, plugins.Base, error) {
	path, err := resolve(i.loader.dir, i.driver)
	if err != nil {
		return nil, nil, err
	}

	cmd := exec.Command(path, i.args...)
	cmd.Env = LegacyHandshakeEnv()
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          pluginMap(i.pluginType),
		Cmd:              cmd,
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

	if err := base.SetConfig(config); err != nil {
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
		i.noteCall(raw, err)
	}()
	return fn(raw)
}

// noteCall counts the calls to the process of raw that ran out of the call
// timeout, and stops the process once hungAfter of them came in a row.
func (i *instance) noteCall(raw plugins.Base, err error) {
	i.l.Lock()
	defer i.l.Unlock()
	if i.raw != raw || i.client == nil {
		return // the process was replaced meanwhile
	}
	if !isCallTimeout(err) {
		i.timeouts = 0
		return
	}
	i.timeouts++
	if i.timeouts < hungAfter {
		return
	}
	i.logger.Warn("external plugin does not answer, stopping it to start it again",
		"timeouts", i.timeouts, "call_timeout", CallTimeout)
	metrics.IncrCounterWithLabels([]string{"nomad", "autoscaler", "plugin", "hung"}, 1,
		[]metrics.Label{{Name: "plugin_name", Value: i.name}})
	i.client.Kill()
	i.client, i.raw, i.timeouts = nil, nil, 0
	i.failedLocked(i.loader.now(), false)
}

// isCallTimeout reports whether a call ran out of CallTimeout.
func isCallTimeout(err error) bool {
	return err != nil && (errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded)
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
