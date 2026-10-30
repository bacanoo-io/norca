// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	cstate "github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// testMTLSLifecycle records the change mode actions of the mtls hook.
type testMTLSLifecycle struct {
	lock     sync.Mutex
	running  bool
	restarts []*structs.TaskEvent
	signals  []string
	events   []*structs.TaskEvent
}

func (l *testMTLSLifecycle) Restart(_ context.Context, event *structs.TaskEvent, failure bool) error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if failure {
		panic("a change mode restart must not count as a failure")
	}
	l.restarts = append(l.restarts, event)
	return nil
}

func (l *testMTLSLifecycle) Signal(event *structs.TaskEvent, signal string) error {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.signals = append(l.signals, signal)
	l.events = append(l.events, event)
	return nil
}

func (l *testMTLSLifecycle) Kill(context.Context, *structs.TaskEvent) error {
	panic("unexpected kill")
}

func (l *testMTLSLifecycle) IsRunning() bool {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.running
}

func (l *testMTLSLifecycle) setRunning(running bool) {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.running = running
}

// actions returns the messages of the restarts and the signals sent.
func (l *testMTLSLifecycle) actions() ([]string, []string) {
	l.lock.Lock()
	defer l.lock.Unlock()
	var restarts []string
	for _, e := range l.restarts {
		restarts = append(restarts, e.DisplayMessage)
	}
	return restarts, append([]string(nil), l.signals...)
}

func waitFor(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(f), wait.Timeout(timeout), wait.Gap(50*time.Millisecond)))
}

// startMTLSHook runs the prestart of the hook for a task that then runs.
func startMTLSHook(t *testing.T, rpc *fakeMTLSServer, m *structs.ServiceMTLS) (*mtlsHook, *testMTLSLifecycle) {
	lc := &testMTLSLifecycle{}
	h, req := newTestMTLSHookWith(t, rpc, m, lc)
	must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	t.Cleanup(func() { h.Stop(context.Background(), nil, nil) })
	lc.setRunning(true)
	return h, lc
}

func TestMTLSHook_ChangeMode_RenewalRestarts(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, 3*time.Second)
	_, lc := startMTLSHook(t, rpc, &structs.ServiceMTLS{Mode: structs.MTLSModeApp, ChangeMode: structs.MTLSChangeModeRestart})

	// The first certificate, written during prestart, is no change.
	restarts, _ := lc.actions()
	must.SliceEmpty(t, restarts)

	waitFor(t, 10*time.Second, func() bool {
		restarts, _ := lc.actions()
		return len(restarts) >= 1
	})
	restarts, signals := lc.actions()
	must.Eq(t, "mTLS certificate renewed", restarts[0])
	must.SliceEmpty(t, signals)
	lc.lock.Lock()
	must.Eq(t, structs.TaskRestartSignal, lc.restarts[0].Type)
	lc.lock.Unlock()
}

func TestMTLSHook_ChangeMode_RenewalSignals(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, 3*time.Second)
	m := &structs.ServiceMTLS{Mode: structs.MTLSModeApp, ChangeMode: structs.MTLSChangeModeSignal, ChangeSignal: "SIGHUP"}
	_, lc := startMTLSHook(t, rpc, m)

	waitFor(t, 10*time.Second, func() bool {
		_, signals := lc.actions()
		return len(signals) >= 1
	})
	restarts, signals := lc.actions()
	must.SliceEmpty(t, restarts)
	must.Eq(t, "SIGHUP", signals[0])
	lc.lock.Lock()
	must.Eq(t, structs.TaskSignaling, lc.events[0].Type)
	must.Eq(t, "mTLS certificate renewed", lc.events[0].DisplayMessage)
	lc.lock.Unlock()
}

// A CA rotation adds a CA to the bundle and re-issues the certificate a few
// seconds later: the task is restarted once.
func TestMTLSHook_ChangeMode_RotationCoalesced(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, time.Hour)
	h, lc := startMTLSHook(t, rpc, &structs.ServiceMTLS{ChangeMode: structs.MTLSChangeModeRestart})
	before := rpc.signCount()

	rpc.rotate("root-2")
	waitFor(t, 15*time.Second, func() bool {
		restarts, _ := lc.actions()
		return len(restarts) >= 1
	})
	must.Greater(t, before, rpc.signCount())
	h.lock.Lock()
	must.Eq(t, "root-2", h.cert.rootID)
	h.lock.Unlock()

	// No second action follows.
	time.Sleep(time.Second)
	restarts, _ := lc.actions()
	must.Eq(t, []string{"mTLS certificate renewed and trust bundle rotated"}, restarts)
}

// A new CA of another region changes only the bundle.
func TestMTLSHook_ChangeMode_ChangeOn(t *testing.T) {
	ci.Parallel(t)

	t.Run("bundle", func(t *testing.T) {
		ci.Parallel(t)
		rpc := newFakeMTLSServer(t, 3*time.Second)
		m := &structs.ServiceMTLS{ChangeMode: structs.MTLSChangeModeSignal, ChangeSignal: "SIGUSR1", ChangeOn: structs.MTLSChangeOnBundle}
		_, lc := startMTLSHook(t, rpc, m)

		// Renewals don't signal.
		waitFor(t, 10*time.Second, func() bool { return rpc.signCount() >= 2 })
		time.Sleep(300 * time.Millisecond)
		_, signals := lc.actions()
		must.SliceEmpty(t, signals)

		rpc.addCA("eu-root", false)
		waitFor(t, 10*time.Second, func() bool {
			_, signals := lc.actions()
			return len(signals) == 1
		})
		// A renewal at the same moment joins the signal.
		lc.lock.Lock()
		must.StrContains(t, lc.events[0].DisplayMessage, "trust bundle rotated")
		lc.lock.Unlock()
	})

	t.Run("certificate", func(t *testing.T) {
		ci.Parallel(t)
		rpc := newFakeMTLSServer(t, time.Hour)
		m := &structs.ServiceMTLS{ChangeMode: structs.MTLSChangeModeRestart, ChangeOn: structs.MTLSChangeOnCertificate}
		h, lc := startMTLSHook(t, rpc, m)

		rpc.addCA("eu-root", false)
		waitFor(t, 10*time.Second, func() bool {
			data, err := os.ReadFile(filepath.Join(h.dir, mtlsCAFile))
			return err == nil && strings.Count(string(data), "BEGIN CERTIFICATE") == 2
		})
		time.Sleep(500 * time.Millisecond)
		restarts, _ := lc.actions()
		must.SliceEmpty(t, restarts)

		// A rotation re-issues the certificate.
		rpc.rotate("root-2")
		waitFor(t, 15*time.Second, func() bool {
			restarts, _ := lc.actions()
			return len(restarts) == 1
		})
	})
}

func TestMTLSHook_ChangeMode_Noop(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, 2*time.Second)
	_, lc := startMTLSHook(t, rpc, nil)

	waitFor(t, 10*time.Second, func() bool { return rpc.signCount() >= 2 })
	rpc.addCA("eu-root", false)
	time.Sleep(time.Second)
	restarts, signals := lc.actions()
	must.SliceEmpty(t, restarts)
	must.SliceEmpty(t, signals)
}

// Changes are not applied while the task isn't running, and a prestart drops
// the changes the starting task doesn't need.
func TestMTLSHook_ChangeMode_NotRunning(t *testing.T) {
	ci.Parallel(t)

	t.Run("not applied", func(t *testing.T) {
		ci.Parallel(t)
		rpc := newFakeMTLSServer(t, 3*time.Second)
		lc := &testMTLSLifecycle{}
		h, req := newTestMTLSHookWith(t, rpc, &structs.ServiceMTLS{ChangeMode: structs.MTLSChangeModeRestart}, lc)
		must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
		defer h.Stop(context.Background(), nil, nil)

		// The task never runs: renewals are not applied.
		waitFor(t, 10*time.Second, func() bool { return rpc.signCount() >= 3 })
		restarts, _ := lc.actions()
		must.SliceEmpty(t, restarts)
	})

	t.Run("dropped by prestart", func(t *testing.T) {
		ci.Parallel(t)
		rpc := newFakeMTLSServer(t, time.Hour)
		lc := &testMTLSLifecycle{}
		h, req := newTestMTLSHookWith(t, rpc, &structs.ServiceMTLS{ChangeMode: structs.MTLSChangeModeRestart}, lc)
		h.changeCoalesce = time.Second
		must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
		defer h.Stop(context.Background(), nil, nil)
		lc.setRunning(true)

		// The task restarts before the change is applied: its prestart
		// drops the change, the task reads the new files when it starts.
		h.changed(mtlsChangeCert)
		lc.setRunning(false)
		must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
		h.changeLock.Lock()
		must.Eq(t, mtlsChange(0), h.pendingChanges)
		h.changeLock.Unlock()
		lc.setRunning(true)
		time.Sleep(1500 * time.Millisecond)
		restarts, _ := lc.actions()
		must.SliceEmpty(t, restarts)
	})
}

// After a client restart the restored task keeps its valid certificate; a CA
// added while the client was down is a change for it.
func TestMTLSHook_ChangeMode_Restore(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, time.Hour)
	lc := &testMTLSLifecycle{}
	m := &structs.ServiceMTLS{ChangeMode: structs.MTLSChangeModeSignal, ChangeSignal: "SIGHUP"}
	h, req := newTestMTLSHookWith(t, rpc, m, lc)
	must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	h.Shutdown()
	lc.setRunning(true)
	must.Eq(t, 1, rpc.signCount())

	// A new hook, as after a client restart, with the same files.
	restored := newMTLSHook(&mtlsHookConfig{
		alloc: h.alloc, task: h.alloc.Job.TaskGroups[0].Tasks[0], rpc: rpc, region: "global",
		nodeKey: "node-secret", events: &mockEmitter{}, lifecycle: lc, logger: h.logger,
	})
	restored.changeCoalesce, restored.changeSplay = 50*time.Millisecond, 0
	resp := &interfaces.TaskPrestartResponse{}
	must.NoError(t, restored.Prestart(context.Background(), req, resp))
	must.Eq(t, 1, rpc.signCount())
	must.Eq(t, "spiffe://global.nomad/ns/default/job/example/group/web", resp.Env["NOMAD_MTLS_SPIFFE_ID"])
	restored.lock.Lock()
	must.Eq(t, "root-1", restored.cert.rootID)
	restored.lock.Unlock()
	time.Sleep(300 * time.Millisecond)
	_, signals := lc.actions()
	must.SliceEmpty(t, signals)
	restored.Shutdown()

	// A CA rotation while the client was down: new bundle and certificate.
	rpc.rotate("root-2")
	again := newMTLSHook(&mtlsHookConfig{
		alloc: h.alloc, task: h.alloc.Job.TaskGroups[0].Tasks[0], rpc: rpc, region: "global",
		nodeKey: "node-secret", events: &mockEmitter{}, lifecycle: lc, logger: h.logger,
	})
	again.changeCoalesce, again.changeSplay = 50*time.Millisecond, 0
	must.NoError(t, again.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	defer again.Stop(context.Background(), nil, nil)
	must.Eq(t, 2, rpc.signCount())
	waitFor(t, 5*time.Second, func() bool {
		_, signals := lc.actions()
		return len(signals) == 1
	})
	lc.lock.Lock()
	must.Eq(t, "mTLS certificate renewed and trust bundle rotated", lc.events[0].DisplayMessage)
	lc.lock.Unlock()
}

// An in-place update changes the change mode of the running hook.
func TestMTLSHook_ChangeMode_Update(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, 3*time.Second)
	h, lc := startMTLSHook(t, rpc, nil)

	alloc := h.alloc.Copy()
	alloc.Job.TaskGroups[0].Services[0].MTLS = &structs.ServiceMTLS{
		Mode: structs.MTLSModeApp, ChangeMode: structs.MTLSChangeModeSignal, ChangeSignal: "SIGUSR2"}
	must.NoError(t, h.Update(context.Background(), &interfaces.TaskUpdateRequest{Alloc: alloc}, nil))

	waitFor(t, 10*time.Second, func() bool {
		_, signals := lc.actions()
		return len(signals) >= 1
	})
	_, signals := lc.actions()
	must.Eq(t, "SIGUSR2", signals[0])
}

func TestMTLSBundleAdded(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, time.Hour)
	one := []byte(rpc.cas["root-1"].pem)
	rpc.addCA("root-2", false)
	two := append(append([]byte{}, one...), rpc.cas["root-2"].pem...)

	must.True(t, mtlsBundleAdded(nil, one))
	must.True(t, mtlsBundleAdded(one, two))
	must.False(t, mtlsBundleAdded(two, one))
	must.False(t, mtlsBundleAdded(two, two))
	must.False(t, mtlsBundleAdded(one, nil))
}

func TestMTLSChange_String(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "mTLS certificate renewed", mtlsChangeCert.String())
	must.Eq(t, "mTLS trust bundle rotated", mtlsChangeBundle.String())
	must.Eq(t, "mTLS certificate renewed and trust bundle rotated", (mtlsChangeCert | mtlsChangeBundle).String())
}

// TestTaskRunner_MTLSChangeMode runs a task with the mock driver: a CA
// rotation restarts it once, and a client restart neither re-issues the
// valid certificate nor restarts the task.
func TestTaskRunner_MTLSChangeMode(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.BatchAlloc()
	alloc.Job.TaskGroups[0].Count = 1
	tg := alloc.Job.TaskGroups[0]
	task := tg.Tasks[0]
	task.Driver = "mock_driver"
	task.Config = map[string]interface{}{"run_for": "60s"}
	tg.Services = []*structs.Service{{
		Name: "web", Provider: structs.ServiceProviderNomad,
		MTLS: &structs.ServiceMTLS{Mode: structs.MTLSModeApp, ChangeMode: structs.MTLSChangeModeRestart},
	}}
	alloc.SignedIdentities = map[string]string{task.Name: "jwt"}

	rpc := newFakeMTLSServer(t, time.Hour)
	conf, cleanup := secretTestTaskRunnerConfig(t, alloc, task)
	defer cleanup()
	conf.RPCClient = rpc
	conf.ClientConfig.Region = "global"
	conf.ClientConfig.Node.SecretID = "node-secret"
	conf.StateDB = cstate.NewMemDB(conf.Logger)

	tr, err := NewTaskRunner(conf)
	must.NoError(t, err)
	go tr.Run()
	defer tr.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	testWaitForTaskToStart(t, tr)
	must.Eq(t, 1, rpc.signCount())
	must.FileExists(t, filepath.Join(conf.TaskDir.SecretsDir, "mtls", "cert.pem"))

	events := func(tr *TaskRunner, typ string) []*structs.TaskEvent {
		var out []*structs.TaskEvent
		for _, ev := range tr.TaskState().Events {
			if ev.Type == typ {
				out = append(out, ev)
			}
		}
		return out
	}

	rpc.rotate("root-2")
	waitFor(t, 30*time.Second, func() bool {
		return len(events(tr, structs.TaskStarted)) == 2 && tr.TaskState().State == structs.TaskStateRunning
	})
	restarts := events(tr, structs.TaskRestartSignal)
	must.Len(t, 1, restarts)
	must.Eq(t, "mTLS certificate renewed and trust bundle rotated", restarts[0].DisplayMessage)
	must.Eq(t, 2, rpc.signCount())

	// Restart the client.
	tr.Shutdown()
	newTR, err := NewTaskRunner(conf)
	must.NoError(t, err)
	must.NoError(t, newTR.Restore())
	go newTR.Run()
	defer newTR.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	time.Sleep(8 * time.Second)
	must.Eq(t, structs.TaskStateRunning, newTR.TaskState().State)
	must.Eq(t, 2, rpc.signCount())
	must.Len(t, 1, events(newTR, structs.TaskRestartSignal))
	must.Len(t, 2, events(newTR, structs.TaskStarted))
}
