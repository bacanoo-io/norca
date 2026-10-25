// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/allocrunner/taskrunner/getter"
	"github.com/hashicorp/nomad/client/config"
	consulapi "github.com/hashicorp/nomad/client/consul"
	"github.com/hashicorp/nomad/client/devicemanager"
	"github.com/hashicorp/nomad/client/pluginmanager/drivermanager"
	"github.com/hashicorp/nomad/client/secrets"
	regMock "github.com/hashicorp/nomad/client/serviceregistration/mock"
	"github.com/hashicorp/nomad/client/serviceregistration/wrapper"
	cstate "github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// secretTestCanary is a secret value the tests search for everywhere a
// value must never appear.
const secretTestCanary = "canary-91c2e-t4sk-s3cr3t"

// testSecretProvider is an in-memory provider with change notification.
type testSecretProvider struct {
	name   string
	audits bool

	lock    sync.Mutex
	items   map[string]string
	version int
	err     error
	changed chan struct{}
	fetches int
}

func newTestSecretProvider(name string, items map[string]string) *testSecretProvider {
	p := &testSecretProvider{name: name, audits: true, changed: make(chan struct{})}
	if items != nil {
		p.items, p.version = items, 1
	}
	return p
}

func (p *testSecretProvider) set(items map[string]string, err error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if items != nil {
		p.items = items
		p.version++
	}
	p.err = err
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *testSecretProvider) fetchCount() int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.fetches
}

func (p *testSecretProvider) Name() string { return p.name }
func (p *testSecretProvider) Type() string { return "test" }
func (p *testSecretProvider) Capabilities() secrets.Capabilities {
	return secrets.Capabilities{Versions: true, Watch: true, AuditsReads: p.audits}
}

func (p *testSecretProvider) Fetch(ctx context.Context, req *secrets.Request) (*secrets.Secret, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.fetches++
	if p.err != nil {
		return nil, p.err
	}
	if p.items == nil {
		return nil, secrets.NewError(p.name, secrets.KindNotFound, errors.New("secret not found"))
	}
	items := make(map[string]string, len(p.items))
	for k, v := range p.items {
		items[k] = v
	}
	return &secrets.Secret{Items: items, Version: strconv.Itoa(p.version)}, nil
}

func (p *testSecretProvider) Watch(ctx context.Context, req *secrets.Request, version string) (string, error) {
	for {
		p.lock.Lock()
		current, err, ch := strconv.Itoa(p.version), p.err, p.changed
		p.lock.Unlock()
		if err != nil {
			return "", err
		}
		if current != version {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ch:
		}
	}
}

// testSecretLifecycle records the change mode actions.
type testSecretLifecycle struct {
	lock     sync.Mutex
	running  bool
	restarts int
	signals  []string
	kills    int
}

func (l *testSecretLifecycle) Restart(ctx context.Context, event *structs.TaskEvent, failure bool) error {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.restarts++
	return nil
}

func (l *testSecretLifecycle) Signal(event *structs.TaskEvent, signal string) error {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.signals = append(l.signals, signal)
	return nil
}

func (l *testSecretLifecycle) Kill(ctx context.Context, event *structs.TaskEvent) error {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.kills++
	return nil
}

func (l *testSecretLifecycle) IsRunning() bool {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.running
}

func (l *testSecretLifecycle) counts() (int, []string) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.restarts, append([]string(nil), l.signals...)
}

type testSecretEvents struct {
	lock   sync.Mutex
	events []*structs.TaskEvent
}

func (e *testSecretEvents) EmitEvent(ev *structs.TaskEvent) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.events = append(e.events, ev)
}

func (e *testSecretEvents) messages() []string {
	e.lock.Lock()
	defer e.lock.Unlock()
	var out []string
	for _, ev := range e.events {
		out = append(out, ev.DisplayMessage)
	}
	return out
}

type testSecretAuditor struct {
	lock    sync.Mutex
	records []string
}

func (a *testSecretAuditor) Record(id *secrets.Identity, provider, path, version string, err error) error {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.records = append(a.records, provider+":"+path+":"+secrets.AuditResult(err))
	return nil
}

type secretHookTest struct {
	hook      *secretHook
	lifecycle *testSecretLifecycle
	events    *testSecretEvents
	auditor   *testSecretAuditor
	env       *taskenv.Builder
	taskDir   *allocdir.TaskDir
	alloc     *structs.Allocation
	task      *structs.Task
}

func newSecretHookTest(t *testing.T, reg *secrets.Registry, specs ...*structs.TaskSecret) *secretHookTest {
	t.Helper()
	alloc := mock.Alloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.User = ""
	task.Driver = "mock_driver"
	task.Secrets = specs
	for _, s := range specs {
		s.Canonicalize()
		must.NoError(t, s.Validate())
	}
	dir := t.TempDir()
	must.NoError(t, os.MkdirAll(filepath.Join(dir, "secrets"), 0o777))
	ht := &secretHookTest{
		lifecycle: &testSecretLifecycle{},
		events:    &testSecretEvents{},
		auditor:   &testSecretAuditor{},
		env:       taskenv.NewBuilder(mock.Node(), alloc, task, "global"),
		taskDir:   &allocdir.TaskDir{Dir: dir, SecretsDir: filepath.Join(dir, "secrets")},
		alloc:     alloc,
		task:      task,
	}
	ht.hook = ht.newHook(reg)
	return ht
}

func (ht *secretHookTest) newHook(reg *secrets.Registry) *secretHook {
	return newSecretHook(&secretHookConfig{
		alloc:      ht.alloc,
		task:       ht.task,
		registry:   reg,
		auditor:    ht.auditor,
		lifecycle:  ht.lifecycle,
		events:     ht.events,
		envBuilder: ht.env,
		logger:     hclog.NewNullLogger(),
		nomadToken: func() string { return "wi-token" },
	})
}

func (ht *secretHookTest) prestart(ctx context.Context) (*interfaces.TaskPrestartResponse, error) {
	req := &interfaces.TaskPrestartRequest{Task: ht.task, TaskDir: ht.taskDir}
	resp := &interfaces.TaskPrestartResponse{}
	err := ht.hook.Prestart(ctx, req, resp)
	return resp, err
}

func dbSecret(mode string) *structs.TaskSecret {
	return &structs.TaskSecret{
		Name:       "db",
		Path:       "jobs/${job}/db",
		File:       &structs.TaskSecretFile{Destination: "secrets/db.env"},
		Env:        map[string]string{"DB_PASSWORD": "password"},
		ChangeMode: mode,
		Timeout:    5 * time.Second,
	}
}

func TestSecretHook_DeliverFileAndEnv(t *testing.T) {
	ci.Parallel(t)
	p := newTestSecretProvider("norca", map[string]string{"password": secretTestCanary, "username": "app"})
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), dbSecret(""))
	defer ht.hook.Shutdown()

	resp, err := ht.prestart(context.Background())
	must.NoError(t, err)

	// The file: atomic write, mode 0400, env format.
	dest := filepath.Join(ht.taskDir.Dir, "secrets", "db.env")
	data, err := os.ReadFile(dest)
	must.NoError(t, err)
	must.Eq(t, "password=\""+secretTestCanary+"\"\nusername=\"app\"\n", string(data))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dest)
		must.NoError(t, err)
		must.Eq(t, os.FileMode(0o400), fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Join(ht.taskDir.Dir, "secrets"))
	must.NoError(t, err)
	must.Len(t, 1, entries, must.Sprint("no temporary files left"))

	// The environment: on the builder only, never in the (persisted)
	// response of the hook.
	must.Eq(t, secretTestCanary, ht.env.Build().All()["DB_PASSWORD"])
	must.MapEmpty(t, resp.Env)
	must.MapEmpty(t, resp.State)

	// The resolved provider is in the task events, the value never is.
	msgs := ht.events.messages()
	must.SliceContains(t, msgs, "secret db: provider norca (fallback)")
	must.StrNotContains(t, strings.Join(msgs, "\n"), secretTestCanary)

	// The built-in store audits its reads on the servers: no client record.
	must.SliceEmpty(t, ht.auditor.records)
}

func TestSecretHook_PrestartBlocksUntilDelivered(t *testing.T) {
	ci.Parallel(t)
	p := newTestSecretProvider("norca", nil)
	p.err = secrets.NewError("norca", secrets.KindUnavailable, errors.New("no servers"))
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), dbSecret(""))
	defer ht.hook.Shutdown()

	done := make(chan error, 1)
	go func() {
		_, err := ht.prestart(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("prestart returned before the secret was available: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	must.SliceContains(t, ht.events.messages(), "secret db: waiting for provider norca: provider norca: no servers")

	p.set(map[string]string{"password": "pw"}, nil)
	select {
	case err := <-done:
		must.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("prestart did not return after the secret became available")
	}
}

func TestSecretHook_TimeoutAndErrors(t *testing.T) {
	ci.Parallel(t)

	// An unreachable provider fails the task at the timeout.
	p := newTestSecretProvider("norca", nil)
	p.err = secrets.NewError("norca", secrets.KindUnavailable, errors.New("no servers"))
	spec := dbSecret("")
	spec.Timeout = time.Second
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	start := time.Now()
	_, err := ht.prestart(context.Background())
	must.ErrorContains(t, err, "secret db: not delivered within 1s: provider norca: no servers")
	must.Less(t, 10*time.Second, time.Since(start))
	ht.hook.Shutdown()

	// A denial fails immediately, with the provider's error.
	p = newTestSecretProvider("norca", nil)
	p.err = secrets.NewError("norca", secrets.KindDenied, errors.New(structs.ErrPermissionDenied.Error()))
	ht = newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), dbSecret(""))
	_, err = ht.prestart(context.Background())
	must.ErrorContains(t, err, "secret db: provider norca: Permission denied")
	ht.hook.Shutdown()

	// A defined provider the client doesn't have fails; no fallback.
	p = newTestSecretProvider("norca", map[string]string{"password": "pw"})
	spec = dbSecret("")
	spec.Provider = "aws"
	ht = newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	_, err = ht.prestart(context.Background())
	must.ErrorContains(t, err, `provider "aws" (explicit) is not configured on this client`)
	must.Eq(t, 0, p.fetchCount())
	ht.hook.Shutdown()

	// An optional secret doesn't hold the task up.
	p = newTestSecretProvider("norca", nil)
	p.err = secrets.NewError("norca", secrets.KindUnavailable, errors.New("no servers"))
	spec = dbSecret("")
	spec.Optional = true
	ht = newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	_, err = ht.prestart(context.Background())
	must.NoError(t, err)
	ht.hook.Shutdown()
}

func TestSecretHook_NamespaceDefaultAndAudit(t *testing.T) {
	ci.Parallel(t)
	norca := newTestSecretProvider("norca", map[string]string{"password": "built-in"})
	bao := newTestSecretProvider("bao", map[string]string{"password": "external"})
	bao.audits = false
	reg := secrets.NewRegistry(norca, nil, nil)
	reg.Register(bao)

	ht := newSecretHookTest(t, reg, dbSecret(""))
	ht.alloc.Job.Secrets = &structs.SecretsConfig{NamespaceDefaultProvider: "bao"}
	defer ht.hook.Shutdown()
	_, err := ht.prestart(context.Background())
	must.NoError(t, err)
	must.Eq(t, "external", ht.env.Build().All()["DB_PASSWORD"])
	must.SliceContains(t, ht.events.messages(), "secret db: provider bao (namespace default)")
	must.Eq(t, 0, norca.fetchCount())

	// The client records the fetch from the external provider.
	must.Eq(t, []string{"bao:jobs/" + ht.alloc.JobID + "/db:ok"}, ht.auditor.records)
}

func TestSecretHook_ChangeModes(t *testing.T) {
	ci.Parallel(t)

	for _, mode := range []string{"restart", "signal", "noop"} {
		t.Run(mode, func(t *testing.T) {
			p := newTestSecretProvider("norca", map[string]string{"password": "one"})
			spec := dbSecret(mode)
			if mode == "signal" {
				spec.ChangeSignal = "SIGHUP"
			}
			ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
			defer ht.hook.Shutdown()
			_, err := ht.prestart(context.Background())
			must.NoError(t, err)
			ht.lifecycle.lock.Lock()
			ht.lifecycle.running = true
			ht.lifecycle.lock.Unlock()

			dest := filepath.Join(ht.taskDir.Dir, "secrets", "db.env")
			start := time.Now()
			p.set(map[string]string{"password": "two"}, nil)
			must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
				data, _ := os.ReadFile(dest)
				return bytes.Contains(data, []byte("two"))
			}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
			must.Less(t, 10*time.Second, time.Since(start))
			must.Eq(t, "two", ht.env.Build().All()["DB_PASSWORD"])

			must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
				restarts, signals := ht.lifecycle.counts()
				switch mode {
				case "restart":
					return restarts == 1
				case "signal":
					return len(signals) == 1 && signals[0] == "SIGHUP"
				}
				return true
			}), wait.Timeout(5*time.Second), wait.Gap(50*time.Millisecond)))
			if mode == "noop" {
				time.Sleep(200 * time.Millisecond)
				restarts, signals := ht.lifecycle.counts()
				must.Eq(t, 0, restarts)
				must.SliceEmpty(t, signals)
			}

			// An unreachable provider keeps the delivered file.
			p.set(nil, secrets.NewError("norca", secrets.KindUnavailable, errors.New("no servers")))
			time.Sleep(300 * time.Millisecond)
			data, err := os.ReadFile(dest)
			must.NoError(t, err)
			must.StrContains(t, string(data), "two")
		})
	}
}

func TestSecretHook_RestoredTaskDoesNotBlock(t *testing.T) {
	ci.Parallel(t)
	p := newTestSecretProvider("norca", map[string]string{"password": "one"})
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), dbSecret(""))
	_, err := ht.prestart(context.Background())
	must.NoError(t, err)
	ht.hook.Shutdown()

	// The client restarts while the provider is unreachable: the running
	// task keeps its file and the hook doesn't block.
	p.set(nil, secrets.NewError("norca", secrets.KindUnavailable, errors.New("no servers")))
	ht.lifecycle.running = true
	ht.hook = ht.newHook(secrets.NewRegistry(p, nil, nil))
	defer ht.hook.Shutdown()
	done := make(chan error, 1)
	go func() {
		_, err := ht.prestart(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		must.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("prestart of a restored task blocked")
	}
	data, err := os.ReadFile(filepath.Join(ht.taskDir.Dir, "secrets", "db.env"))
	must.NoError(t, err)
	must.StrContains(t, string(data), "one")

	// Once the provider is back, the value is fetched again.
	fetches := p.fetchCount()
	p.set(map[string]string{"password": "one"}, nil)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return p.fetchCount() > fetches && ht.env.Build().All()["DB_PASSWORD"] == "one"
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
}

func TestSecretHook_RedactSecretEnv(t *testing.T) {
	ci.Parallel(t)
	task := &structs.Task{Secrets: []*structs.TaskSecret{dbSecret("")}}
	handle := &drivers.TaskHandle{Config: &drivers.TaskConfig{
		Env: map[string]string{"DB_PASSWORD": secretTestCanary, "OTHER": "x"},
	}}
	out := redactSecretEnv(handle, task)
	must.Eq(t, map[string]string{"OTHER": "x"}, out.Config.Env)
	must.Eq(t, secretTestCanary, handle.Config.Env["DB_PASSWORD"], must.Sprint("the live handle is unchanged"))
	must.Eq(t, handle, redactSecretEnv(handle, &structs.Task{}))
}

// TestTaskRunner_SecretDelivery runs a task with the mock driver and checks
// that the secret reaches the task's file and environment, that the value
// is never in the job, the allocation, the task events, the client state
// database or the client log, and that a restarted client fetches it again.
func TestTaskRunner_SecretDelivery(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.BatchAlloc()
	alloc.Job.TaskGroups[0].Count = 1
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Driver = "mock_driver"
	task.Config = map[string]interface{}{"run_for": "30s"}
	task.Secrets = []*structs.TaskSecret{dbSecret("")}
	task.Secrets[0].Canonicalize()

	p := newTestSecretProvider("norca", map[string]string{"password": secretTestCanary})
	conf, cleanup := secretTestTaskRunnerConfig(t, alloc, task)
	defer cleanup()
	logs := &bytes.Buffer{}
	var logLock sync.Mutex
	conf.Logger = hclog.New(&hclog.LoggerOptions{Level: hclog.Trace, Output: &lockedWriter{w: logs, lock: &logLock}})
	stateDir := t.TempDir()
	db, err := cstate.NewBoltStateDB(conf.Logger, stateDir)
	must.NoError(t, err)
	conf.StateDB = db
	conf.SecretProviders = secrets.NewRegistry(p, nil, nil)

	tr, err := NewTaskRunner(conf)
	must.NoError(t, err)
	go tr.Run()
	defer tr.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	testWaitForTaskToStart(t, tr)

	data, err := os.ReadFile(filepath.Join(conf.TaskDir.Dir, "secrets", "db.env"))
	must.NoError(t, err)
	must.StrContains(t, string(data), secretTestCanary)
	must.Eq(t, secretTestCanary, tr.envBuilder.Build().All()["DB_PASSWORD"])
	must.Eq(t, 1, p.fetchCount())

	// Restart the client: the new task runner fetches the value again.
	tr.Shutdown()
	newTR, err := NewTaskRunner(conf)
	must.NoError(t, err)
	must.NoError(t, newTR.Restore())
	go newTR.Run()
	defer newTR.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	testutil.WaitForResult(func() (bool, error) {
		return p.fetchCount() == 2 && newTR.envBuilder.Build().All()["DB_PASSWORD"] == secretTestCanary, nil
	}, func(error) {
		t.Fatalf("restored task runner did not fetch the secret again (%d fetches)", p.fetchCount())
	})

	// The canary is nowhere but in the delivered file and environment.
	outputs := map[string]any{
		"job":         alloc.Job,
		"alloc":       newTR.Alloc(),
		"task state":  newTR.TaskState(),
		"local state": newTR.localState,
	}
	for name, out := range outputs {
		b, err := json.Marshal(out)
		must.NoError(t, err)
		must.StrNotContains(t, string(b), secretTestCanary, must.Sprint(name))
	}
	must.NoError(t, db.Close())
	dbFiles, err := filepath.Glob(filepath.Join(stateDir, "*"))
	must.NoError(t, err)
	must.SliceNotEmpty(t, dbFiles)
	for _, f := range dbFiles {
		b, err := os.ReadFile(f)
		must.NoError(t, err)
		must.False(t, bytes.Contains(b, []byte(secretTestCanary)), must.Sprintf("client state %s", f))
	}
	logLock.Lock()
	defer logLock.Unlock()
	must.Positive(t, logs.Len())
	must.StrNotContains(t, logs.String(), secretTestCanary)
}

// secretTestTaskRunnerConfig is testTaskRunnerConfig without the cgroup of
// the task, which an unprivileged test can't create; the mock driver needs
// none.
func secretTestTaskRunnerConfig(t *testing.T, alloc *structs.Allocation, task *structs.Task) (*Config, func()) {
	logger := testlog.HCLogger(t)
	clientConf, cleanup := config.TestClientConfig(t)
	allocDir := allocdir.NewAllocDir(logger, clientConf.AllocDir, alloc.ID)
	must.NoError(t, allocDir.Build())
	closedCh := make(chan struct{})
	close(closedCh)
	shutdownDelayCtx, shutdownDelayCancelFn := context.WithCancel(context.Background())
	wrapperMock := wrapper.NewHandlerWrapper(logger,
		regMock.NewServiceRegistrationHandler(logger), regMock.NewServiceRegistrationHandler(logger))
	conf := &Config{
		Alloc:                 alloc,
		ClientConfig:          clientConf,
		Task:                  task,
		TaskDir:               allocDir.NewTaskDir(task.Name),
		Logger:                clientConf.Logger,
		Consul:                regMock.NewServiceRegistrationHandler(logger),
		ConsulSI:              consulapi.NewMockServiceIdentitiesClient(),
		Vault:                 vaultclient.NewMockVaultClient(),
		StateDB:               cstate.NoopDB{},
		StateUpdater:          NewMockTaskStateUpdater(),
		DeviceManager:         devicemanager.NoopMockManager(),
		DriverManager:         drivermanager.TestDriverManager(t),
		ServersContactedCh:    make(chan struct{}),
		StartConditionMetCh:   closedCh,
		ShutdownDelayCtx:      shutdownDelayCtx,
		ShutdownDelayCancelFn: shutdownDelayCancelFn,
		ServiceRegWrapper:     wrapperMock,
		Getter:                getter.TestSandbox(t),
	}
	return conf, func() {
		_ = allocDir.Destroy()
		cleanup()
	}
}

type lockedWriter struct {
	w    *bytes.Buffer
	lock *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.w.Write(p)
}
