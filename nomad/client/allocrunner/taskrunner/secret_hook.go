// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/consul-template/signals"
	log "github.com/hashicorp/go-hclog"

	"github.com/bacanoo-io/norca/nomad/client/allocrunner/interfaces"
	ti "github.com/bacanoo-io/norca/nomad/client/allocrunner/taskrunner/interfaces"
	"github.com/bacanoo-io/norca/nomad/client/config"
	"github.com/bacanoo-io/norca/nomad/client/secrets"
	"github.com/bacanoo-io/norca/nomad/client/taskenv"
	"github.com/bacanoo-io/norca/nomad/helper"
	"github.com/bacanoo-io/norca/nomad/helper/users"
	"github.com/bacanoo-io/norca/nomad/helper/uuid"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/bacanoo-io/norca/nomad/plugins/drivers"
)

// The secret hook delivers the task's secret blocks (Norca,
// norca-doc content/docs/design/secrets-plan.md, "Delivery to tasks"): for each secret it
// resolves the provider, fetches the value with the task's workload
// identity, writes the file into the task's secrets directory and/or puts
// the items into the task's environment, and watches for changes, which are
// re-delivered according to the change mode.
//
// A dynamic secret (engine and role, phase 7) is a credential created for
// the task with a lease: the hook renews the lease while the task runs (at
// half of its remaining time), asks for a new credential once the lease
// can't be extended any more (max_ttl) or was revoked, delivers it with the
// change mode, and revokes the lease when the task stops. The servers revoke
// the leases of stopped allocations and unrenewed leases themselves.
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

	// secretFallbackAttempts is how many times a credential is requested
	// before a task starts with the fallback of its secret block.
	secretFallbackAttempts = 3

	// secretWaitEventInterval rate limits the events of a secret that is
	// still being waited for.
	secretWaitEventInterval = 30 * time.Second

	// secretLeaseMinWait bounds the waits of the lease loop from below.
	secretLeaseMinWait = 100 * time.Millisecond

	// secretRevokeTimeout bounds the revocation of a lease at the task's
	// stop.
	secretRevokeTimeout = 5 * time.Second
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

	lock     sync.Mutex
	started  bool
	restored bool
	states   []*secretState
	envs     map[string]map[string]string
	exec     ti.ScriptExecutor
	taskDir  string
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

	// lease is the lease of the delivered credential of a dynamic secret;
	// leaseLock guards it, as the task's stop revokes it.
	leaseLock sync.Mutex
	lease     *secrets.Lease

	// resumed is set for a dynamic secret of a restored task whose lease
	// was found in the hook's persisted state: the delivered file stays,
	// and the lease is renewed.
	resumed bool

	// keepFallback is set for a restored task that stays on its fallback.
	keepFallback bool

	// onFallback is set while the fallback is delivered; provider and
	// manager serve the request of a credential when the task restarts.
	// leaseLock guards them.
	onFallback bool
	provider   secrets.Provider
	manager    secrets.LeaseManager
}

// setFallback records that the fallback is delivered.
func (st *secretState) setFallback(p secrets.Provider, lm secrets.LeaseManager) {
	st.leaseLock.Lock()
	defer st.leaseLock.Unlock()
	st.onFallback, st.provider, st.manager = true, p, lm
}

// fallback returns the provider if the fallback is delivered.
func (st *secretState) fallback() (secrets.Provider, secrets.LeaseManager, bool) {
	st.leaseLock.Lock()
	defer st.leaseLock.Unlock()
	return st.provider, st.manager, st.onFallback
}

// leaveFallback ends the fallback.
func (st *secretState) leaveFallback() {
	st.leaseLock.Lock()
	defer st.leaseLock.Unlock()
	st.onFallback = false
}

// currentLease returns the lease of the delivered credential.
func (st *secretState) currentLease() *secrets.Lease {
	st.leaseLock.Lock()
	defer st.leaseLock.Unlock()
	return st.lease
}

func (st *secretState) setLease(l *secrets.Lease) {
	st.leaseLock.Lock()
	defer st.leaseLock.Unlock()
	st.lease = l
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

// secretLeaseStatePrefix prefixes the hook state keys of the leases of the
// dynamic secrets: the lease ID and its expiry, never the credential. A
// restored task resumes renewing them.
const secretLeaseStatePrefix = "lease."

// secretFallbackStatePrefix marks in the hook state a secret delivered from
// its fallback.
const secretFallbackStatePrefix = "fallback."

// encodeSecretLease and decodeSecretLease convert a lease to and from its
// hook state value.
func encodeSecretLease(l *secrets.Lease) string {
	return fmt.Sprintf("%s %d %d", l.ID, l.ExpireTime.UnixNano(), l.MaxExpireTime.UnixNano())
}

func decodeSecretLease(v string) *secrets.Lease {
	parts := strings.Fields(v)
	if len(parts) != 3 {
		return nil
	}
	expire, err1 := strconv.ParseInt(parts[1], 10, 64)
	maxExpire, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || parts[0] == "" {
		return nil
	}
	return &secrets.Lease{
		ID:            parts[0],
		Renewable:     expire < maxExpire,
		ExpireTime:    time.Unix(0, expire),
		MaxExpireTime: time.Unix(0, maxExpire),
	}
}

// Prestart starts the delivery of every secret on the first run and blocks
// until every required secret is delivered or has failed. A task restored
// after a client restart is already running with its delivered files: its
// secrets are fetched again in the background, without blocking, and the
// leases of its dynamic secrets found in the hook state are renewed.
func (h *secretHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	h.lock.Lock()
	restored := false
	started := h.started
	if !h.started {
		h.started = true
		h.taskDir = req.TaskDir.Dir
		restored = h.config.lifecycle.IsRunning()
		h.restored = restored
		jobID := structs.SecretJobID(h.config.alloc.Job)
		for _, spec := range h.config.task.Secrets {
			st := &secretState{
				spec:    spec,
				path:    structs.InterpolateSecretPath(spec.Path, jobID, h.config.alloc.TaskGroup, h.config.task.Name),
				initial: make(chan struct{}),
			}
			if spec.Engine != "" {
				// Routes and events refer to the credentials path.
				st.path = structs.SecretEngineCredsPath(spec.Engine, spec.Role)
				if restored {
					if l := decodeSecretLease(req.PreviousState[secretLeaseStatePrefix+spec.Name]); l != nil {
						st.lease, st.resumed = l, true
					}
					st.keepFallback = spec.FallbackKeepOnRestart && req.PreviousState[secretFallbackStatePrefix+spec.Name] != ""
				}
			}
			h.states = append(h.states, st)
			go h.run(st)
		}
	}
	// A later run is a restart of the task: the hook runs on, with what it
	// delivered.
	restarted := started && !h.config.lifecycle.IsRunning()
	states := h.states
	h.lock.Unlock()

	if restored {
		resp.State = maps.Clone(req.PreviousState)
		return nil
	}
	if restarted {
		for _, st := range states {
			h.retryFallback(st)
		}
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
	for _, st := range states {
		if l := st.currentLease(); l != nil {
			if resp.State == nil {
				resp.State = map[string]string{}
			}
			resp.State[secretLeaseStatePrefix+st.spec.Name] = encodeSecretLease(l)
		}
		if _, _, on := st.fallback(); on {
			if resp.State == nil {
				resp.State = map[string]string{}
			}
			resp.State[secretFallbackStatePrefix+st.spec.Name] = "1"
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

// Stop ends the deliveries and revokes the leases of the task's dynamic
// secrets. Shutdown (the agent stops, the task keeps running) does not.
func (h *secretHook) Stop(ctx context.Context, req *interfaces.TaskStopRequest, resp *interfaces.TaskStopResponse) error {
	h.cancel()
	h.lock.Lock()
	states := h.states
	h.lock.Unlock()
	for _, st := range states {
		h.revoke(st)
	}
	return nil
}

// revoke revokes the lease of a dynamic secret, if any. A failure is
// logged: the servers revoke the lease once the allocation stopped.
func (h *secretHook) revoke(st *secretState) {
	lease := st.currentLease()
	if lease == nil {
		return
	}
	res, err := h.config.registry.Resolve(secrets.LookupFor(h.config.alloc.Job, h.config.alloc.TaskGroup, st.spec.Provider, st.path))
	if err != nil {
		return
	}
	lm, ok := res.Provider.(secrets.LeaseManager)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretRevokeTimeout)
	defer cancel()
	if err := lm.Revoke(ctx, lease, h.identity()); err != nil {
		h.logger.Warn("failed to revoke the lease of a dynamic secret; the servers revoke it",
			"secret", st.spec.Name, "lease_id", lease.ID, "error", err)
		return
	}
	st.setLease(nil)
	h.logger.Debug("revoked the lease of a dynamic secret", "secret", st.spec.Name, "lease_id", lease.ID)
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
	if st.spec.Engine != "" {
		req.Path, req.Engine, req.Role, req.Name = "", st.spec.Engine, st.spec.Role, st.spec.Name
	}
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

	var leases secrets.LeaseManager
	if spec.Engine != "" {
		lm, ok := p.(secrets.LeaseManager)
		if !ok || !p.Capabilities().Leases {
			err := fmt.Errorf("provider %s does not support dynamic secrets (engine, role)", p.Name())
			h.event("secret %s: %v", spec.Name, err)
			fail(fmt.Errorf("secret %s: %w", spec.Name, err))
			return
		}
		leases = lm
		if st.keepFallback {
			// The task was started with the fallback and stays on it.
			h.event("secret %s: staying on the fallback", spec.Name)
			st.setFallback(p, lm)
			st.done(nil)
			return
		}
		if st.resumed {
			// The task runs with the delivered credential; keep its
			// lease alive, or replace it if it ended meanwhile.
			h.event("secret %s: resuming lease %s", spec.Name, st.currentLease().ID)
			st.done(nil)
			h.leaseLoop(st, p, leases)
			return
		}
	}

	// First delivery.
	deadline := time.Now().Add(spec.Timeout)
	var lastEvent time.Time
	var attempts uint64
	for {
		sec, err := h.fetch(st, p)
		fetchFailed := err != nil
		if err == nil {
			if err = h.deliver(st, sec); err == nil {
				h.event("secret %s: delivered%s", spec.Name, deliveredSuffix(sec))
				if !st.done(nil) {
					// An optional secret delivered after its timeout is
					// a change of the running task.
					h.changed(st)
				} else if sec.Lease != nil && h.isRestored() {
					// After a client restart without a persisted lease
					// the task still uses a credential whose lease the
					// client no longer renews; the servers revoke it.
					h.changed(st)
				}
				break
			}
		}
		if h.ctx.Err() != nil {
			fail(h.ctx.Err())
			return
		}
		if fetchFailed && spec.Fallback != "" && secrets.KindOf(err) == secrets.KindUnavailable &&
			attempts+1 >= secretFallbackAttempts && h.deliverFallback(st, p, err) {
			st.setFallback(p, leases)
			st.done(nil)
			if spec.FallbackRecover == structs.SecretFallbackRecoverSignal {
				h.recoverFromFallback(st, p, leases)
			}
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

	if leases != nil {
		h.leaseLoop(st, p, leases)
		return
	}
	// A pinned version never changes.
	if spec.Version != 0 {
		return
	}
	h.watch(st, p)
}

func (h *secretHook) isRestored() bool {
	h.lock.Lock()
	defer h.lock.Unlock()
	return h.restored
}

// deliveredSuffix describes a delivered secret for the task events: its
// version, or its lease.
func deliveredSuffix(sec *secrets.Secret) string {
	if l := sec.Lease; l != nil {
		return fmt.Sprintf(" (lease %s, until %s at most)", l.ID, l.MaxExpireTime.UTC().Format(time.RFC3339))
	}
	return versionSuffix(sec.Version)
}

// leaseLoop keeps the credential of a dynamic secret alive until the task
// stops: it renews the lease at half of its remaining time, and asks for a
// new credential (delivered with the change mode) when the lease can't be
// extended any more, at half of the time left before its max_ttl, or when
// a renewal finds it revoked. Failed requests are retried with a backoff.
func (h *secretHook) leaseLoop(st *secretState, p secrets.Provider, lm secrets.LeaseManager) {
	spec := st.spec
	var attempts uint64
	var lastEvent time.Time
	reissue := false
	var retryAt time.Time

	report := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if time.Since(lastEvent) >= secretWaitEventInterval {
			lastEvent = time.Now()
			h.event("secret %s: %s", spec.Name, msg)
		}
		h.logger.Warn("dynamic secret", "secret", spec.Name, "provider", p.Name(), "error", msg)
	}
	backoff := func() time.Time {
		d := helper.Backoff(secretBackoffBaseline, secretBackoffLimit, attempts)
		attempts++
		return time.Now().Add(d)
	}

	for h.ctx.Err() == nil {
		lease := st.currentLease()
		now := time.Now()
		var at time.Time
		switch {
		case !retryAt.IsZero():
			at = retryAt
		case lease == nil:
			reissue, at = true, now
		case !lease.ExpireTime.Before(lease.MaxExpireTime):
			// The lease ends at its max_ttl: a new credential is needed.
			reissue, at = true, now.Add(lease.MaxExpireTime.Sub(now)/2)
		default:
			at = now.Add(lease.ExpireTime.Sub(now) / 2)
		}
		wait := max(time.Until(at), secretLeaseMinWait)
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(wait):
		}
		retryAt = time.Time{}

		if !reissue {
			renewed, err := lm.Renew(h.ctx, lease, h.identity())
			if h.ctx.Err() != nil {
				return
			}
			if err == nil {
				attempts = 0
				if items := renewed.NewItems; len(items) > 0 {
					// The renewal replaced the credential.
					renewed.NewItems = nil
					if err := h.deliver(st, &secrets.Secret{Items: items, Version: renewed.ID, Lease: renewed}); err != nil {
						report("failed to deliver the renewed credential: %v", err)
						st.setLease(renewed)
						continue
					}
					h.event("secret %s: renewed credential delivered", spec.Name)
					h.changed(st)
					continue
				}
				st.setLease(renewed)
				h.logger.Trace("renewed the lease of a dynamic secret", "secret", spec.Name, "lease_id", renewed.ID)
				continue
			}
			switch secrets.KindOf(err) {
			case secrets.KindNotFound, secrets.KindDenied:
				// Revoked or expired: a new credential is needed.
				h.event("secret %s: lease %s ended (%v); requesting a new credential", spec.Name, lease.ID, err)
				reissue = true
			default:
				report("failed to renew lease %s: %v", lease.ID, err)
				retryAt = backoff()
				if lease.ExpireTime.Before(retryAt) {
					// It would expire before the retry.
					reissue = true
					retryAt = time.Time{}
				}
				continue
			}
		}

		sec, err := h.fetch(st, p)
		if h.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = h.deliver(st, sec)
		}
		if err != nil {
			report("failed to get a new credential: %v (keeping the delivered credential)", err)
			retryAt = backoff()
			continue
		}
		attempts, reissue = 0, false
		h.event("secret %s: new credential delivered%s", spec.Name, deliveredSuffix(sec))
		h.changed(st)
	}
}

// deliverFallback delivers the static fallback of a dynamic secret and
// reports whether it succeeded. The reason of the failed credential stays in
// the task events: running on a fallback must be visible.
func (h *secretHook) deliverFallback(st *secretState, p secrets.Provider, reason error) bool {
	spec := st.spec
	path := structs.InterpolateSecretPath(spec.Fallback, structs.SecretJobID(h.config.alloc.Job),
		h.config.alloc.TaskGroup, h.config.task.Name)
	sec, err, aerr := secrets.FetchAudited(h.ctx, p, &secrets.Request{Path: path, Identity: h.identity()}, h.config.auditor)
	if aerr != nil {
		h.logger.Warn("failed to record the audit record of a secret fetch",
			"secret", spec.Name, "provider", p.Name(), "error", aerr)
	}
	if err == nil {
		err = h.deliver(st, sec)
	}
	if err != nil {
		h.logger.Warn("failed to deliver the fallback of a secret", "secret", spec.Name, "error", err)
		return false
	}
	metrics.IncrCounterWithLabels([]string{"client", "secrets", "fallback"}, 1,
		[]metrics.Label{{Name: "engine", Value: spec.Engine}})
	h.event("secret %s: provider fallback, static secret %s delivered (%v)", spec.Name, path, reason)
	return true
}

// retryFallback asks for a credential once when a task on a fallback
// restarts, before the task runs. With fallback_recover the running loop does
// this, and fallback_keep_on_restart keeps the fallback. If it fails, the task
// starts with the fallback again.
func (h *secretHook) retryFallback(st *secretState) {
	spec := st.spec
	p, lm, on := st.fallback()
	if !on || spec.FallbackKeepOnRestart || spec.FallbackRecover != "" {
		return
	}
	sec, err := h.fetch(st, p)
	if err == nil {
		err = h.deliver(st, sec)
	}
	if err != nil {
		h.event("secret %s: staying on the fallback after the restart: %v", spec.Name, err)
		return
	}
	st.leaveFallback()
	h.event("secret %s: credential delivered, leaving the fallback%s", spec.Name, deliveredSuffix(sec))
	go h.leaseLoop(st, p, lm)
}

// recoverFromFallback asks for a credential until one is delivered, applies
// the change mode and goes on with the lease.
func (h *secretHook) recoverFromFallback(st *secretState, p secrets.Provider, lm secrets.LeaseManager) {
	spec := st.spec
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(spec.FallbackRecoverInterval):
		}
		sec, err := h.fetch(st, p)
		if h.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = h.deliver(st, sec)
		}
		if err != nil {
			h.logger.Debug("still on the fallback of a secret", "secret", spec.Name, "error", err)
			continue
		}
		st.leaveFallback()
		h.event("secret %s: credential delivered, leaving the fallback%s", spec.Name, deliveredSuffix(sec))
		h.changed(st)
		h.leaseLoop(st, p, lm)
		return
	}
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
// A dynamic secret without a file or env block is written to the provider's
// default file; its lease becomes the secret's lease.
func (h *secretHook) deliver(st *secretState, sec *secrets.Secret) error {
	spec := st.spec
	for _, f := range spec.Files {
		data, err := secrets.Render(f, sec.Items)
		if err != nil {
			return fmt.Errorf("secret %s: %v", spec.Name, err)
		}
		if err := writeSecretFile(h.taskDir, f.Destination, data, h.config.task); err != nil {
			return fmt.Errorf("secret %s: failed to write %s: %v", spec.Name, f.Destination, err)
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
	if sec.Lease != nil {
		st.setLease(sec.Lease)
	}
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
// task's user where the agent may change the owner. destination is the
// validated slash-separated path below secrets/, relative to taskDir.
//
// The task can write to its secrets directory, so it can plant symlinks
// there. Every access goes through an os.Root opened on the task directory,
// which never resolves a path outside of it, and the directories on the way
// to the file must be real directories: a symlink (or junction) among them is
// refused rather than followed. A symlink at the destination itself is
// replaced by the rename, not followed.
func writeSecretFile(taskDir, destination string, data []byte, task *structs.Task) error {
	parts := strings.Split(destination, "/")
	if len(parts) < 2 {
		return fmt.Errorf("invalid secret file destination %q", destination)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `\:`) {
			return fmt.Errorf("invalid secret file destination %q", destination)
		}
	}

	root, err := os.OpenRoot(taskDir)
	if err != nil {
		return err
	}
	defer root.Close()

	dir := ""
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		fi, err := root.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			fi, err = root.Lstat(dir)
		}
		if err != nil {
			return err
		}
		if fi.Mode().Type() != fs.ModeDir {
			return fmt.Errorf("%s is not a directory (symlinks are not followed)", filepath.ToSlash(dir))
		}
	}

	dest := filepath.Join(dir, parts[len(parts)-1])
	tmp := filepath.Join(dir, "."+parts[len(parts)-1]+".tmp-"+uuid.Generate()[:8])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if uid, gid, ok := secretFileOwner(task); ok {
		// Changing the owner needs privileges; an unprivileged agent runs
		// its tasks as its own user, which owns the file already.
		_ = f.Chown(uid, gid)
	}
	if err := f.Chmod(0o400); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Windows refuses to replace a read-only file: make the previous
		// version writable first (a regular file only, never a link).
		if fi, err := root.Lstat(dest); err == nil && fi.Mode().IsRegular() {
			_ = root.Chmod(dest, 0o600)
		}
	}
	return root.Rename(tmp, dest)
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
