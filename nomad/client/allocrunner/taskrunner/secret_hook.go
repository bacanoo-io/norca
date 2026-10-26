// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/consul-template/signals"
	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	ti "github.com/hashicorp/nomad/client/allocrunner/taskrunner/interfaces"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/secrets"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/users"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// The secret hook delivers the task's secret blocks (Norca,
// docs/design/secrets-plan.md, "Delivery to tasks"): for each secret it
// resolves the provider, fetches the value with the task's workload
// identity, writes the file into the task's secrets directory and/or puts
// the items into the task's environment, and watches for changes, which are
// re-delivered according to the change mode.
//
// Values stay in memory: environment values are set on the environment
// builder directly (the hook's response environment is persisted in the
// client's state) and are removed from the persisted driver handle
// (redactSecretEnv); task events and logs only name the secret, the
// provider and the version.

const (
	secretHookName = "secret"

	// secretEnvHookName is the environment builder key of the delivered
	// variables. It differs from the hook name: the task runner replaces
	// the environment of a hook's name with the hook's (persisted)
	// response environment.
	secretEnvHookName = "secret-env"

	// secretTaskEventType is the type of the hook's task events.
	secretTaskEventType = "Secret"

	// secretBackoffBaseline and secretBackoffLimit bound the retries of
	// failed fetches and watches.
	secretBackoffBaseline = 1 * time.Second
	secretBackoffLimit    = 30 * time.Second

	// secretWaitEventInterval rate limits the events of a secret that is
	// still being waited for.
	secretWaitEventInterval = 30 * time.Second
)

type secretHookConfig struct {
	alloc      *structs.Allocation
	task       *structs.Task
	registry   *secrets.Registry
	auditor    secrets.Auditor
	lifecycle  ti.TaskLifecycle
	events     ti.EventEmitter
	envBuilder *taskenv.Builder
	logger     log.Logger

	// nomadToken and vaultToken return the task's current workload
	// identity and Vault token.
	nomadToken func() string
	vaultToken func() string
}

type secretHook struct {
	config *secretHookConfig
	logger log.Logger

	ctx    context.Context
	cancel context.CancelFunc

	lock    sync.Mutex
	started bool
	states  []*secretState
	envs    map[string]map[string]string
	exec    ti.ScriptExecutor
	taskDir string
}

// secretState is the delivery state of one secret block.
type secretState struct {
	spec *structs.TaskSecret
	path string

	// initial is closed once the first delivery succeeded or failed for
	// good; initErr is the failure.
	initial     chan struct{}
	initErr     error
	initialOnce sync.Once

	// version and fingerprint describe the delivered value.
	version     string
	fingerprint string
}

// done ends the wait for the first delivery; it reports whether it was
// still waited for.
func (st *secretState) done(err error) bool {
	first := false
	st.initialOnce.Do(func() {
		first = true
		st.initErr = err
		close(st.initial)
	})
	return first
}

func newSecretHook(config *secretHookConfig) *secretHook {
	ctx, cancel := context.WithCancel(context.Background())
	h := &secretHook{
		config: config,
		ctx:    ctx,
		cancel: cancel,
		envs:   map[string]map[string]string{},
	}
	h.logger = config.logger.Named(secretHookName)
	return h
}

func (*secretHook) Name() string { return secretHookName }

// Prestart starts the delivery of every secret on the first run and blocks
// until every required secret is delivered or has failed. A task restored
// after a client restart is already running with its delivered files: its
// secrets are fetched again in the background, without blocking.
func (h *secretHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	h.lock.Lock()
	restored := false
	if !h.started {
		h.started = true
		h.taskDir = req.TaskDir.Dir
		restored = h.config.lifecycle.IsRunning()
		jobID := structs.SecretJobID(h.config.alloc.Job)
		for _, spec := range h.config.task.Secrets {
			st := &secretState{
				spec:    spec,
				path:    structs.InterpolateSecretPath(spec.Path, jobID, h.config.alloc.TaskGroup, h.config.task.Name),
				initial: make(chan struct{}),
			}
			h.states = append(h.states, st)
			go h.run(st)
		}
	}
	states := h.states
	h.lock.Unlock()

	if restored {
		return nil
	}
	for _, st := range states {
		if st.spec.Optional {
			continue
		}
		select {
		case <-st.initial:
			if st.initErr != nil {
				return st.initErr
			}
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// Poststart keeps the driver's exec capability for change_mode script.
func (h *secretHook) Poststart(ctx context.Context, req *interfaces.TaskPoststartRequest, resp *interfaces.TaskPoststartResponse) error {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.exec = req.DriverExec
	if req.DriverExec == nil {
		for _, s := range h.config.task.Secrets {
			if s.ChangeMode == structs.SecretChangeModeScript {
				return fmt.Errorf("secret %q has change mode script but the task driver does not provide exec", s.Name)
			}
		}
	}
	return nil
}

func (h *secretHook) Stop(ctx context.Context, req *interfaces.TaskStopRequest, resp *interfaces.TaskStopResponse) error {
	h.cancel()
	return nil
}

func (h *secretHook) Shutdown() {
	h.cancel()
}

// identity returns the workload the task's lookups are made for.
func (h *secretHook) identity() *secrets.Identity {
	return secretIdentity(h.config.alloc, h.config.task, h.config.nomadToken, h.config.vaultToken)
}

// secretIdentity returns the identity of a task's secret lookups.
func secretIdentity(alloc *structs.Allocation, task *structs.Task, nomadToken, vaultToken func() string) *secrets.Identity {
	id := &secrets.Identity{
		Namespace: alloc.Namespace,
		JobID:     alloc.JobID,
		Group:     alloc.TaskGroup,
		Task:      task.Name,
		AllocID:   alloc.ID,
	}
	if nomadToken != nil {
		id.Token = nomadToken()
	}
	if vaultToken != nil {
		id.VaultToken = vaultToken()
	}
	if v := task.Vault; v != nil {
		id.VaultNamespace = v.Namespace
	}
	return id
}

// secretTemplateSource returns the source of the task's nomadSecret
// template calls.
func (tr *TaskRunner) secretTemplateSource(logger log.Logger) *secrets.TemplateSource {
	alloc, task := tr.Alloc(), tr.Task()
	jobID := structs.SecretJobID(alloc.Job)
	return secrets.NewTemplateSource(&secrets.TemplateSourceConfig{
		Registry: tr.secretRegistry(),
		Auditor:  tr.secretAuditRecorder(),
		Logger:   logger.Named("nomad_secret"),
		Lookup: func(explicit, path string) *secrets.Lookup {
			return secrets.LookupFor(alloc.Job, alloc.TaskGroup, explicit, path)
		},
		Identity: func() *secrets.Identity {
			return secretIdentity(alloc, task, tr.getNomadToken, tr.getVaultToken)
		},
		Interpolate: func(p string) string {
			return structs.InterpolateSecretPath(p, jobID, alloc.TaskGroup, task.Name)
		},
		Vault: tr.secretVaultReader(),
	})
}

// secretVaultReader returns the reader of the task's Vault-style lookups
// served by the secret providers (phase 4, "Vault compatibility"): nil when
// the agent has a Vault integration, which keeps serving them as in Nomad,
// or when the Vault compatibility is disabled (vault_compat enabled = false).
func (tr *TaskRunner) secretVaultReader() *secrets.VaultReader {
	if !tr.clientConfig.VaultCompatEnabled() {
		return nil
	}
	var rules []*config.VaultKVRule
	if tr.clientConfig.Secrets != nil {
		rules = tr.clientConfig.Secrets.VaultKV
	}
	return &secrets.VaultReader{
		Registry: tr.secretRegistry(),
		Mapper:   secrets.NewVaultMapper(rules),
		Auditor:  tr.secretAuditRecorder(),
	}
}

func (h *secretHook) event(format string, args ...any) {
	h.config.events.EmitEvent(structs.NewTaskEvent(secretTaskEventType).
		SetDisplayMessage(fmt.Sprintf(format, args...)))
}

// fetch fetches the secret once and records the client audit record for
// providers that don't audit reads themselves.
func (h *secretHook) fetch(st *secretState, p secrets.Provider) (*secrets.Secret, error) {
	req := &secrets.Request{Path: st.path, Version: st.spec.Version, Identity: h.identity()}
	sec, err, aerr := secrets.FetchAudited(h.ctx, p, req, h.config.auditor)
	if aerr != nil {
		h.logger.Warn("failed to record the audit record of a secret fetch",
			"secret", st.spec.Name, "provider", p.Name(), "error", aerr)
	}
	return sec, err
}

// run delivers one secret: the first delivery within the timeout, then the
// re-deliveries of changes until the task stops.
func (h *secretHook) run(st *secretState) {
	spec := st.spec
	fail := func(err error) { st.done(err) }

	res, err := h.config.registry.Resolve(secrets.LookupFor(h.config.alloc.Job, h.config.alloc.TaskGroup, spec.Provider, st.path))
	if err != nil {
		h.event("secret %s: %v", spec.Name, err)
		fail(fmt.Errorf("secret %s: %w", spec.Name, err))
		return
	}
	p := res.Provider
	h.event("secret %s: %s", spec.Name, res)
	h.logger.Debug("secret provider resolved", "secret", spec.Name, "provider", p.Name(), "source", res.Source)

	// First delivery.
	deadline := time.Now().Add(spec.Timeout)
	var lastEvent time.Time
	var attempts uint64
	for {
		sec, err := h.fetch(st, p)
		if err == nil {
			if err = h.deliver(st, sec); err == nil {
				h.event("secret %s: delivered%s", spec.Name, versionSuffix(st.version))
				if !st.done(nil) {
					// An optional secret delivered after its timeout is
					// a change of the running task.
					h.changed(st)
				}
				break
			}
		}
		if h.ctx.Err() != nil {
			fail(h.ctx.Err())
			return
		}
		recoverable := secrets.IsRecoverable(err)
		if !recoverable || (time.Now().After(deadline) && !spec.Optional) {
			msg := err.Error()
			if recoverable {
				msg = fmt.Sprintf("not delivered within %s: %v", spec.Timeout, err)
			}
			h.logger.Error("failed to deliver secret", "secret", spec.Name, "provider", p.Name(), "error", msg)
			if spec.Optional {
				h.event("secret %s: optional secret not delivered: %s", spec.Name, msg)
				st.done(nil)
				return
			}
			h.event("secret %s: %s", spec.Name, msg)
			fail(fmt.Errorf("secret %s: %s", spec.Name, msg))
			return
		}
		if time.Since(lastEvent) >= secretWaitEventInterval {
			lastEvent = time.Now()
			h.event("secret %s: waiting for provider %s: %v", spec.Name, p.Name(), err)
		}
		if spec.Optional && time.Now().After(deadline) {
			// An optional secret no longer holds the task up; it is
			// delivered (as a change) once it can be fetched.
			st.done(nil)
		}
		backoff := helper.Backoff(secretBackoffBaseline, secretBackoffLimit, attempts)
		attempts++
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(backoff):
		}
	}

	// A pinned version never changes.
	if spec.Version != 0 {
		return
	}
	h.watch(st, p)
}

// watch re-delivers the secret when it changes, through the provider's
// watch or by polling. Errors keep the delivered value: a running task
// keeps its files when the provider becomes unreachable.
func (h *secretHook) watch(st *secretState, p secrets.Provider) {
	spec := st.spec
	watcher, canWatch := p.(secrets.Watcher)
	canWatch = canWatch && p.Capabilities().Watch
	interval := p.Capabilities().PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	var attempts uint64
	var lastEvent time.Time
	retry := func(err error) bool {
		if time.Since(lastEvent) >= secretWaitEventInterval {
			lastEvent = time.Now()
			h.event("secret %s: provider %s: %v (keeping the delivered value)", spec.Name, p.Name(), err)
		}
		h.logger.Warn("failed to check secret for changes", "secret", spec.Name, "provider", p.Name(), "error", err)
		backoff := helper.Backoff(secretBackoffBaseline, secretBackoffLimit, attempts)
		attempts++
		select {
		case <-h.ctx.Done():
			return false
		case <-time.After(backoff):
			return true
		}
	}

	for h.ctx.Err() == nil {
		if canWatch {
			v, err := watcher.Watch(h.ctx, &secrets.Request{Path: st.path, Identity: h.identity()}, st.version)
			if err != nil {
				if h.ctx.Err() != nil || !retry(err) {
					return
				}
				continue
			}
			if v == st.version {
				continue
			}
		} else {
			select {
			case <-h.ctx.Done():
				return
			case <-time.After(interval):
			}
		}

		sec, err := h.fetch(st, p)
		if err != nil {
			if h.ctx.Err() != nil || !retry(err) {
				return
			}
			continue
		}
		attempts = 0
		if secrets.Fingerprint(sec.Items) == st.fingerprint {
			st.version = sec.Version
			continue
		}
		if err := h.deliver(st, sec); err != nil {
			if !retry(err) {
				return
			}
			continue
		}
		h.event("secret %s: re-delivered%s", spec.Name, versionSuffix(st.version))
		h.changed(st)
	}
}

func versionSuffix(version string) string {
	if version == "" {
		return ""
	}
	return " version " + version
}

// deliver writes the file and sets the environment variables of a secret.
func (h *secretHook) deliver(st *secretState, sec *secrets.Secret) error {
	spec := st.spec
	if spec.File != nil {
		data, err := secrets.Render(spec.File, sec.Items)
		if err != nil {
			return fmt.Errorf("secret %s: %v", spec.Name, err)
		}
		dest := filepath.Join(h.taskDir, filepath.FromSlash(spec.File.Destination))
		if err := writeSecretFile(dest, data, h.config.task); err != nil {
			return fmt.Errorf("secret %s: failed to write %s: %v", spec.Name, spec.File.Destination, err)
		}
	}
	if len(spec.Env) > 0 {
		vals, err := secrets.EnvValues(spec.Env, sec.Items)
		if err != nil {
			return fmt.Errorf("secret %s: %v", spec.Name, err)
		}
		h.lock.Lock()
		h.envs[spec.Name] = vals
		merged := map[string]string{}
		for _, e := range h.envs {
			maps.Copy(merged, e)
		}
		h.lock.Unlock()
		h.config.envBuilder.SetHookEnv(secretEnvHookName, merged)
	}
	st.version = sec.Version
	st.fingerprint = secrets.Fingerprint(sec.Items)
	return nil
}

// changed applies the change mode of a re-delivered secret to the running
// task.
func (h *secretHook) changed(st *secretState) {
	spec := st.spec
	if !h.config.lifecycle.IsRunning() {
		return
	}
	msg := fmt.Sprintf("secret %s changed", spec.Name)
	switch spec.ChangeMode {
	case structs.SecretChangeModeRestart:
		_ = h.config.lifecycle.Restart(h.ctx,
			structs.NewTaskEvent(structs.TaskRestartSignal).SetDisplayMessage(msg), false)
	case structs.SecretChangeModeSignal:
		sig, err := signals.Parse(spec.ChangeSignal)
		if err != nil {
			h.event("secret %s: invalid change_signal %q", spec.Name, spec.ChangeSignal)
			return
		}
		event := structs.NewTaskEvent(structs.TaskSignaling).SetTaskSignal(sig).SetDisplayMessage(msg)
		if err := h.config.lifecycle.Signal(event, spec.ChangeSignal); err != nil {
			_ = h.config.lifecycle.Kill(h.ctx, structs.NewTaskEvent(structs.TaskKilling).SetFailsTask().
				SetDisplayMessage(fmt.Sprintf("secret %s: failed to send signal: %v", spec.Name, err)))
		}
	case structs.SecretChangeModeScript:
		h.lock.Lock()
		exec := h.exec
		h.lock.Unlock()
		cs := spec.ChangeScript
		fail := func(m string) {
			h.event("secret %s: %s", spec.Name, m)
			if cs.FailOnError {
				_ = h.config.lifecycle.Kill(h.ctx, structs.NewTaskEvent(structs.TaskKilling).SetFailsTask().
					SetDisplayMessage(fmt.Sprintf("secret %s: change script failed, task is being killed", spec.Name)))
			}
		}
		if exec == nil {
			fail("the task driver does not support exec for the change script")
			return
		}
		_, code, err := exec.Exec(cs.Timeout, cs.Command, cs.Args)
		switch {
		case err != nil:
			fail(fmt.Sprintf("change script %s failed: %v", cs.Command, err))
		case code != 0:
			fail(fmt.Sprintf("change script %s exited with code %d", cs.Command, code))
		default:
			h.event("secret %s: change script %s ran", spec.Name, cs.Command)
		}
	}
}

// writeSecretFile writes a secret file atomically (a temporary file in the
// same directory renamed over the destination) with mode 0400, owned by the
// task's user where the agent may change the owner.
func writeSecretFile(dest string, data []byte, task *structs.Task) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if uid, gid, ok := secretFileOwner(task); ok {
		// Changing the owner needs privileges; an unprivileged agent runs
		// its tasks as its own user, which owns the file already.
		_ = os.Chown(tmp, uid, gid)
	}
	if err := os.Chmod(tmp, 0o400); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// secretFileOwner returns the user the task runs as: its user, or nobody
// for the drivers that default to it.
func secretFileOwner(task *structs.Task) (int, int, bool) {
	name := task.User
	if name == "" && (task.Driver == "exec" || task.Driver == "java") {
		name = "nobody"
	}
	if name == "" {
		return 0, 0, false
	}
	u, err := users.Lookup(name)
	if err != nil {
		return 0, 0, false
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, false
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		gid = -1
	}
	return uid, gid, true
}

// redactSecretEnv returns the driver handle to persist in the client's
// state: without the environment variables of the task's secret blocks.
// They are delivered again when the client restarts.
func redactSecretEnv(handle *drivers.TaskHandle, task *structs.Task) *drivers.TaskHandle {
	if handle == nil || handle.Config == nil || task == nil || len(task.Secrets) == 0 {
		return handle
	}
	var keys []string
	for _, s := range task.Secrets {
		for name := range s.Env {
			keys = append(keys, name)
		}
	}
	if len(keys) == 0 {
		return handle
	}
	out := handle.Copy()
	for _, k := range keys {
		delete(out.Config.Env, k)
	}
	return out
}

// secretRegistry returns the secret providers of the task runner: the
// configured override, or the built-in store and the agent's Vault
// integration with the client's routes.
func (tr *TaskRunner) secretRegistry() *secrets.Registry {
	if tr.secretProviders != nil {
		return tr.secretProviders
	}
	var vault secrets.Provider
	if v := secrets.NewVaultProvider(tr.clientConfig.VaultConfig); v != nil {
		vault = v
	}
	var routes []*config.SecretsRoute
	if tr.clientConfig.Secrets != nil {
		routes = tr.clientConfig.Secrets.Routes
	}
	tr.secretProviders = secrets.NewRegistry(secrets.NewNorcaProvider(tr.rpcClient, tr.clientConfig.Region), vault, routes)
	return tr.secretProviders
}

// secretAuditRecorder returns the recorder of client audit records.
func (tr *TaskRunner) secretAuditRecorder() secrets.Auditor {
	if tr.secretAuditor != nil {
		return tr.secretAuditor
	}
	if tr.rpcClient == nil {
		return nil
	}
	tr.secretAuditor = secrets.NewRPCAuditor(tr.rpcClient, tr.clientConfig.Region)
	return tr.secretAuditor
}
