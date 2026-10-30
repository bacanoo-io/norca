// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/consul-template/signals"
	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	ti "github.com/hashicorp/nomad/client/allocrunner/taskrunner/interfaces"
	"github.com/hashicorp/nomad/client/secrets"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

const (
	// vaultBackoffBaseline is the baseline time for exponential backoff when
	// attempting to retrieve a Vault token
	vaultBackoffBaseline = 5 * time.Second

	// vaultBackoffLimit is the limit of the exponential backoff when attempting
	// to retrieve a Vault token
	vaultBackoffLimit = 3 * time.Minute

	// vaultTokenFile is the name of the file holding the Vault token inside the
	// task's secret directory
	vaultTokenFile = "vault_token"
)

type vaultTokenUpdateHandler interface {
	updatedVaultToken(token string)
}

func (tr *TaskRunner) updatedVaultToken(token string) {
	// Update the task runner and environment
	tr.setVaultToken(token)

	// Trigger update hooks with the new Vault token
	tr.triggerUpdateHooks()
}

type vaultHookConfig struct {
	vaultBlock *structs.Vault
	client     vaultclient.VaultClient
	events     ti.EventEmitter
	lifecycle  ti.TaskLifecycle
	updater    vaultTokenUpdateHandler
	logger     log.Logger
	alloc      *structs.Allocation
	task       string
}

type vaultHook struct {
	// vaultBlock is the vault block for the task
	vaultBlock *structs.Vault

	// eventEmitter is used to emit events to the task
	eventEmitter ti.EventEmitter

	// lifecycle is used to signal, restart and kill a task
	lifecycle ti.TaskLifecycle

	// updater is used to update the Vault token
	updater vaultTokenUpdateHandler

	// client is the Vault client to retrieve and renew the Vault token
	client vaultclient.VaultClient

	// logger is used to log
	logger log.Logger

	// ctx and cancel are used to kill the long running token manager
	ctx    context.Context
	cancel context.CancelFunc

	// privateDirTokenPath is the path inside the task's private directory where
	// the Vault token is read and written.
	privateDirTokenPath string

	// secretsDirTokenPath is the path inside the task's secret directory where the
	// Vault token is written unless disabled by the task.
	secretsDirTokenPath string

	// alloc is the allocation
	alloc *structs.Allocation

	// taskName is the name of the task
	taskName string

	// firstRun stores whether it is the first run for the hook
	firstRun bool

	// future is used to wait on retrieving a Vault token
	future *tokenFuture

	// compat is set for a task served by the client's Vault-compatible
	// endpoint (Norca), compatEnv is its environment: VAULT_ADDR.
	compat    bool
	compatEnv map[string]string
}

func newVaultHook(config *vaultHookConfig) *vaultHook {
	ctx, cancel := context.WithCancel(context.Background())
	h := &vaultHook{
		vaultBlock:   config.vaultBlock,
		client:       config.client,
		eventEmitter: config.events,
		lifecycle:    config.lifecycle,
		updater:      config.updater,
		alloc:        config.alloc,
		taskName:     config.task,
		firstRun:     true,
		ctx:          ctx,
		cancel:       cancel,
		future:       newTokenFuture(),
	}
	h.logger = config.logger.Named(h.Name())
	return h
}

func (*vaultHook) Name() string {
	return "vault"
}

func (h *vaultHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	// If we have already run prestart before exit early. We do not use the
	// PrestartDone value because we want to recover the token on restoration.
	first := h.firstRun
	h.firstRun = false
	if !first {
		// Keep the VAULT_ADDR of the Vault-compatible endpoint (Norca).
		resp.Env = h.compatEnv
		return nil
	}

	// Try to recover a token if it was previously written in the secrets
	// directory
	recoveredToken := ""
	h.privateDirTokenPath = filepath.Join(req.TaskDir.PrivateDir, vaultTokenFile)
	h.secretsDirTokenPath = filepath.Join(req.TaskDir.SecretsDir, vaultTokenFile)

	// Handle upgrade path by searching for the previous token in all possible
	// paths where the token may be.
	for _, path := range []string{h.privateDirTokenPath, h.secretsDirTokenPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to recover vault token from %s: %v", path, err)
			}

			// Token file doesn't exist in this path.
		} else {
			// Store the recovered token
			recoveredToken = string(data)
			break
		}
	}

	// Without a Vault integration, the token is one of the client's
	// Vault-compatible endpoint of the built-in secret store (Norca).
	compat, isCompat := h.client.(vaultCompatClient)
	h.compat = isCompat
	if isCompat {
		if msg := secrets.VaultPolicyWarning(h.alloc.Job, h.alloc.TaskGroup, h.taskName, h.vaultBlock.Policies); msg != "" {
			h.eventEmitter.EmitEvent(structs.NewTaskEvent(vaultCompatTaskEventType).SetDisplayMessage(msg))
		}
	}

	// Launch the token manager
	go h.run(recoveredToken)

	// Block until we get a token
	select {
	case <-h.future.Wait():
	case <-ctx.Done():
		return nil
	}

	h.updater.updatedVaultToken(h.future.Get())
	if isCompat {
		h.compatEnv = map[string]string{vaultAddrEnv: compat.VaultAddr(h.alloc)}
		resp.Env = h.compatEnv
	}
	return nil
}

// vaultCompatClient is the Vault client of the client's Vault-compatible
// endpoint (client/vaultcompat), used when the agent has no Vault
// integration (Norca, norca-doc content/docs/design/secrets-plan.md, "Vault compatibility").
type vaultCompatClient interface {
	// VaultAddr returns the endpoint's address for a task of the
	// allocation, which the task gets as VAULT_ADDR.
	VaultAddr(alloc *structs.Allocation) string
}

// redactVaultCompatToken returns the driver handle to persist in the client's
// state without the token of the Vault-compatible endpoint (Norca): the
// token is never kept in the client's state, the client derives it again
// after a restart.
func redactVaultCompatToken(handle *drivers.TaskHandle, client vaultclient.VaultClient) *drivers.TaskHandle {
	if _, ok := client.(vaultCompatClient); !ok || handle == nil || handle.Config == nil {
		return handle
	}
	if _, ok := handle.Config.Env[taskenv.VaultToken]; !ok {
		return handle
	}
	out := handle.Copy()
	delete(out.Config.Env, taskenv.VaultToken)
	return out
}

const (
	// vaultAddrEnv is the environment variable of the Vault address.
	vaultAddrEnv = "VAULT_ADDR"

	// vaultCompatTaskEventType is the type of the warning event of a vault
	// block served by the built-in secret store.
	vaultCompatTaskEventType = "Vault"
)

func (h *vaultHook) Stop(ctx context.Context, req *interfaces.TaskStopRequest, resp *interfaces.TaskStopResponse) error {
	// Shutdown any created manager
	h.cancel()
	return nil
}

func (h *vaultHook) Shutdown() {
	h.cancel()
}

// run should be called in a go-routine and manages the derivation, renewal and
// handling of errors with the Vault token. The optional parameter allows
// setting the initial Vault token. This is useful when the Vault token is
// recovered off disk.
func (h *vaultHook) run(token string) {
	// Helper for stopping token renewal
	stopRenewal := func() {
		if err := h.client.StopRenewToken(h.future.Get()); err != nil {
			h.logger.Warn("failed to stop token renewal", "error", err)
		}
	}

	// updatedToken lets us store state between loops. If true, a new token
	// has been retrieved and we need to apply the Vault change mode
	var updatedToken bool

OUTER:
	for {
		// Check if we should exit
		if h.ctx.Err() != nil {
			stopRenewal()
			return
		}

		// Clear the token
		h.future.Clear()

		// Check if there already is a token which can be the case for
		// restoring the TaskRunner
		if token == "" {
			// Get a token
			var exit bool
			token, exit = h.deriveVaultToken()
			if exit {
				// Exit the manager
				return
			}

			// Write the token to disk
			if err := h.writeToken(token); err != nil {
				errorString := "failed to write Vault token to disk"
				h.logger.Error(errorString, "error", err)
				h.lifecycle.Kill(h.ctx,
					structs.NewTaskEvent(structs.TaskKilling).
						SetFailsTask().
						SetDisplayMessage(fmt.Sprintf("Vault %v", errorString)))
				return
			}
		}

		// Start the renewal process.
		//
		// This is the initial renew of the token which we derived from the
		// server. The client does not know how long it took for the token to
		// be generated and derived and also wants to gain control of the
		// process quickly, but not too quickly. We therefore use a hardcoded
		// increment value of 30; this value without a suffix is in seconds.
		//
		// If Vault is having availability issues or is overloaded, a large
		// number of initial token renews can exacerbate the problem.
		renewCh, err := h.client.RenewToken(token, 30)

		// An error returned means the token is not being renewed
		if err != nil {
			h.logger.Error("failed to start renewal of Vault token", "error", err)
			token = ""
			goto OUTER
		}

		// The Vault token is valid now, so set it
		h.future.Set(token)

		if updatedToken {
			switch h.vaultBlock.ChangeMode {
			case structs.VaultChangeModeSignal:
				s, err := signals.Parse(h.vaultBlock.ChangeSignal)
				if err != nil {
					h.logger.Error("failed to parse signal", "error", err)
					h.lifecycle.Kill(h.ctx,
						structs.NewTaskEvent(structs.TaskKilling).
							SetFailsTask().
							SetDisplayMessage(fmt.Sprintf("Vault: failed to parse signal: %v", err)))
					return
				}

				event := structs.NewTaskEvent(structs.TaskSignaling).SetTaskSignal(s).SetDisplayMessage("Vault: new Vault token acquired")
				if err := h.lifecycle.Signal(event, h.vaultBlock.ChangeSignal); err != nil {
					h.logger.Error("failed to send signal", "error", err)
					h.lifecycle.Kill(h.ctx,
						structs.NewTaskEvent(structs.TaskKilling).
							SetFailsTask().
							SetDisplayMessage(fmt.Sprintf("Vault: failed to send signal: %v", err)))
					return
				}
			case structs.VaultChangeModeRestart:
				const noFailure = false
				h.lifecycle.Restart(h.ctx,
					structs.NewTaskEvent(structs.TaskRestartSignal).
						SetDisplayMessage("Vault: new Vault token acquired"), false)
			case structs.VaultChangeModeNoop:
				fallthrough
			default:
				h.logger.Error("invalid Vault change mode", "mode", h.vaultBlock.ChangeMode)
			}

			// We have handled it
			updatedToken = false

			// Call the handler
			h.updater.updatedVaultToken(token)
		}

		// Start watching for renewal errors
		select {
		case err := <-renewCh:
			// Clear the token
			token = ""
			h.logger.Error("failed to renew Vault token", "error", err)
			stopRenewal()
			updatedToken = true
		case <-h.ctx.Done():
			stopRenewal()
			return
		}
	}
}

// deriveVaultToken derives the Vault token using exponential backoffs. It
// returns the Vault token and whether the manager should exit.
func (h *vaultHook) deriveVaultToken() (token string, exit bool) {
	var attempts uint64
	var backoff time.Duration
	for {
		tokens, err := h.client.DeriveToken(h.alloc, []string{h.taskName})
		if err == nil {
			return tokens[h.taskName], false
		}

		// Check if this is a server side error
		if structs.IsServerSide(err) {
			h.logger.Error("failed to derive Vault token", "error", err, "server_side", true)
			h.lifecycle.Kill(h.ctx,
				structs.NewTaskEvent(structs.TaskKilling).
					SetFailsTask().
					SetDisplayMessage(fmt.Sprintf("Vault: server failed to derive vault token: %v", err)))
			return "", true
		}

		// Check if we can't recover from the error
		if !structs.IsRecoverable(err) {
			h.logger.Error("failed to derive Vault token", "error", err, "recoverable", false)
			h.lifecycle.Kill(h.ctx,
				structs.NewTaskEvent(structs.TaskKilling).
					SetFailsTask().
					SetDisplayMessage(fmt.Sprintf("Vault: failed to derive vault token: %v", err)))
			return "", true
		}

		// Handle the retry case
		backoff = helper.Backoff(vaultBackoffBaseline, vaultBackoffLimit, attempts)
		attempts++

		h.logger.Error("failed to derive Vault token", "error", err, "recoverable", true, "backoff", backoff)

		// Wait till retrying
		select {
		case <-h.ctx.Done():
			return "", true
		case <-time.After(backoff):
		}
	}
}

// writeToken writes the given token to disk
func (h *vaultHook) writeToken(token string) error {
	// The token of the Vault-compatible endpoint (Norca) is not kept in the
	// private directory: the client derives the same token again after a
	// restart, so only the task's own copy is written.
	if h.compat {
		if h.vaultBlock.DisableFile {
			return nil
		}
		if err := os.WriteFile(h.secretsDirTokenPath, []byte(token), 0666); err != nil {
			return fmt.Errorf("failed to write vault token to secrets dir: %v", err)
		}
		return nil
	}

	// Handle upgrade path by first checking if the tasks private directory
	// exists. If it doesn't, this allocation probably existed before the
	// private directory was introduced, so keep using the secret directory to
	// prevent unnecessary errors during task recovery.
	if _, err := os.Stat(path.Dir(h.privateDirTokenPath)); os.IsNotExist(err) {
		if err := os.WriteFile(h.secretsDirTokenPath, []byte(token), 0666); err != nil {
			return fmt.Errorf("failed to write vault token to secrets dir: %v", err)
		}
		return nil
	}

	if err := os.WriteFile(h.privateDirTokenPath, []byte(token), 0600); err != nil {
		return fmt.Errorf("failed to write vault token: %v", err)
	}
	if !h.vaultBlock.DisableFile {
		if err := os.WriteFile(h.secretsDirTokenPath, []byte(token), 0666); err != nil {
			return fmt.Errorf("failed to write vault token to secrets dir: %v", err)
		}
	}

	return nil
}

// tokenFuture stores the Vault token and allows consumers to block till a valid
// token exists
type tokenFuture struct {
	waiting []chan struct{}
	token   string
	set     bool
	m       sync.Mutex
}

// newTokenFuture returns a new token future without any token set
func newTokenFuture() *tokenFuture {
	return &tokenFuture{}
}

// Wait returns a channel that can be waited on. When this channel unblocks, a
// valid token will be available via the Get method
func (f *tokenFuture) Wait() <-chan struct{} {
	f.m.Lock()
	defer f.m.Unlock()

	c := make(chan struct{})
	if f.set {
		close(c)
		return c
	}

	f.waiting = append(f.waiting, c)
	return c
}

// Set sets the token value and unblocks any caller of Wait
func (f *tokenFuture) Set(token string) *tokenFuture {
	f.m.Lock()
	defer f.m.Unlock()

	f.set = true
	f.token = token
	for _, w := range f.waiting {
		close(w)
	}
	f.waiting = nil
	return f
}

// Clear clears the set vault token.
func (f *tokenFuture) Clear() *tokenFuture {
	f.m.Lock()
	defer f.m.Unlock()

	f.token = ""
	f.set = false
	return f
}

// Get returns the set Vault token
func (f *tokenFuture) Get() string {
	f.m.Lock()
	defer f.m.Unlock()
	return f.token
}
