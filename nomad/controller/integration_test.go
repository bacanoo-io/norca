// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// The integration tests run against a real dev agent started from the
// binary named by NORCA_BINARY, or "norca" (or "nomad") on the PATH. The
// controller module cannot import the main module's in-process test server,
// so the agent runs as a separate process. They are skipped with -short or
// when no binary is found.

func agentBinary(t *testing.T) string {
	if testing.Short() {
		t.Skip("integration test skipped with -short")
	}
	if bin := os.Getenv("NORCA_BINARY"); bin != "" {
		return bin
	}
	for _, name := range []string{"norca", "nomad"} {
		if bin, err := exec.LookPath(name); err == nil {
			return bin
		}
	}
	t.Skip("no norca binary: set NORCA_BINARY or put norca on the PATH")
	return ""
}

// freePort returns a port that is free for TCP and UDP (serf uses both).
func freePort(t *testing.T) int {
	// Ephemeral ports are avoided: on Windows many of them are reserved.
	for range 100 {
		port := 20000 + rand.IntN(12000)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		udp, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		ln.Close()
		if err == nil {
			udp.Close()
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

// startAgent starts a dev agent with a small event buffer and returns a
// client for it. Starting is retried, since a port found free can be taken
// (or reserved by the OS) by the time the agent binds it.
func startAgent(t *testing.T, eventBuffer int) *api.Client {
	bin := agentBinary(t)
	for attempt := 1; ; attempt++ {
		client, err := tryStartAgent(t, bin, eventBuffer)
		if err == nil {
			return client
		}
		if attempt == 3 {
			t.Fatalf("starting the agent failed: %v", err)
		}
		t.Logf("starting the agent failed, retrying: %v", err)
	}
}

func tryStartAgent(t *testing.T, bin string, eventBuffer int) (*api.Client, error) {
	dir := t.TempDir()
	httpPort := freePort(t)
	config := fmt.Sprintf(`
bind_addr = "127.0.0.1"
data_dir  = %q
ports {
  http = %d
  rpc  = %d
  serf = %d
}
server {
  event_buffer_size = %d
}
`, filepath.ToSlash(filepath.Join(dir, "data")), httpPort, freePort(t), freePort(t), eventBuffer)
	configFile := filepath.Join(dir, "agent.hcl")
	must.NoError(t, os.WriteFile(configFile, []byte(config), 0o600))
	logFile, err := os.Create(filepath.Join(dir, "agent.log"))
	must.NoError(t, err)

	cmd := exec.Command(bin, "agent", "-dev", "-config", configFile)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	must.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stop := func() {
		_ = cmd.Process.Kill()
		<-exited
		_ = logFile.Close()
	}

	conf := api.DefaultConfig()
	conf.Address = fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	conf.SecretID = ""
	client, err := api.NewClient(conf)
	must.NoError(t, err)

	deadline := time.Now().Add(90 * time.Second)
	for {
		leader, err := client.Status().Leader()
		if err == nil && leader != "" {
			break
		}
		select {
		case <-exited:
			_ = logFile.Close()
			b, _ := os.ReadFile(logFile.Name())
			return nil, fmt.Errorf("agent exited:\n%s", tail(b, 2000))
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			stop()
			return nil, errors.New("no leader within 90s")
		}
	}

	t.Cleanup(func() {
		stop()
		if t.Failed() {
			if b, err := os.ReadFile(logFile.Name()); err == nil && len(b) > 0 {
				t.Logf("agent log (tail):\n%s", tail(b, 4000))
			}
		}
	})
	return client, nil
}
func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// gatedBackend can interrupt the event streams of a backend and hold new
// subscriptions until it is released, like a network partition between a
// controller and the servers.
type gatedBackend struct {
	controller.Backend

	mu      sync.Mutex
	closed  bool
	open    chan struct{}
	cancels []context.CancelFunc
}

func newGated(b controller.Backend) *gatedBackend {
	g := &gatedBackend{Backend: b, open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *gatedBackend) Stream(ctx context.Context, ns string, topics map[api.Topic][]string, index uint64, exact bool) (<-chan *api.Events, error) {
	g.mu.Lock()
	open := g.open
	g.mu.Unlock()
	select {
	case <-open:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(ctx)
	g.mu.Lock()
	g.cancels = append(g.cancels, cancel)
	g.mu.Unlock()
	return g.Backend.Stream(ctx, ns, topics, index, exact)
}

func (g *gatedBackend) cut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = make(chan struct{})
	for _, c := range g.cancels {
		c()
	}
	g.cancels = nil
}

func (g *gatedBackend) restore() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.open)
}

// pendingJob makes widget jobs valid for a real server but unplaceable, so
// no workload runs.
func pendingJob(job *api.Job) {
	job.Constraints = []*api.Constraint{api.NewConstraint("${attr.kernel.name}", "=", "no-such-kernel")}
	job.TaskGroups[0].Tasks = []*api.Task{api.NewTask("t", "raw_exec").SetConfig("command", "unused")}
}

func readWidget(t *testing.T, client *api.Client, name string) (*controller.Object, bool) {
	v, _, err := client.Variables().Peek(widgetKind.Path(name), nil)
	must.NoError(t, err)
	if v == nil {
		return nil, false
	}
	obj, err := controller.ObjectFromVariable(widgetKind, v)
	must.NoError(t, err)
	return obj, true
}

func putWidgetAPI(t *testing.T, client *api.Client, name, spec string) {
	v := api.NewVariable(widgetKind.Path(name))
	v.Items["spec"] = spec
	_, _, err := client.Variables().Create(v, nil)
	must.NoError(t, err)
}

func jobCountAPI(client *api.Client, id string) int {
	job, _, err := client.Jobs().Info(id, nil)
	if err != nil || (job.Stop != nil && *job.Stop) {
		return -1
	}
	return *job.TaskGroups[0].Count
}

func TestIntegration_Controller(t *testing.T) {
	client := startAgent(t, 8)
	backend := newGated(controller.NewBackend(client))
	metrics := controller.NewTextMetrics()

	r := &widgetReconciler{calls: map[string]int{}, fail: map[string]int{}, decorate: pendingJob}
	le := controller.LeaderElectionConfig{
		LeaseDuration: 3 * time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: 500 * time.Millisecond,
		ReleaseOnCancel: true,
	}
	c, err := controller.New(backend, r, controller.Options{
		Name:           "widgets",
		Kind:           widgetKind,
		OwnsJobs:       true,
		LeaderElection: &le,
		Metrics:        metrics,
		Logger:         testLogger(t),
		RateLimiter:    controller.NewExponentialBackoff[controller.ObjectKey](50*time.Millisecond, time.Second),
	})
	must.NoError(t, err)
	r.c = c
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	putWidgetAPI(t, client, "a", `{"size":1}`)
	waitFor(t, "leader", c.IsLeader)
	waitForLong(t, "job created", func() bool { return jobCountAPI(client, "widget-a") == 1 })
	waitForLong(t, "status written", func() bool {
		obj, ok := readWidget(t, client, "a")
		var st widgetStatus
		return ok && obj.DecodeStatus(&st) == nil && st.Size == 1
	})
	job, _, err := client.Jobs().Info("widget-a", nil)
	must.NoError(t, err)
	must.Eq(t, "default/crd/example-com/Widget/a", job.Meta[controller.OwnerMetaKey])

	// The lease is held in a Variable.
	lease, _, err := client.Variables().Read("controller-lease/widgets", nil)
	must.NoError(t, err)
	must.NotEq(t, "", lease.Items["holder"])

	// Spec change through the event stream.
	obj, _ := readWidget(t, client, "a")
	obj.Spec = []byte(`{"size":2}`)
	_, err = c.Objects().Update(context.Background(), obj)
	must.NoError(t, err)
	waitForLong(t, "job updated", func() bool { return jobCountAPI(client, "widget-a") == 2 })

	// Owned job changed by someone else: repaired.
	job, _, err = client.Jobs().Info("widget-a", nil)
	must.NoError(t, err)
	*job.TaskGroups[0].Count = 5
	_, _, err = client.Jobs().Register(job, nil)
	must.NoError(t, err)
	waitForLong(t, "job repaired", func() bool { return jobCountAPI(client, "widget-a") == 2 })

	// Owned job purged: recreated through the Job event of the purge.
	_, _, err = client.Jobs().Deregister("widget-a", true, nil)
	must.NoError(t, err)
	waitForLong(t, "job recreated", func() bool { return jobCountAPI(client, "widget-a") == 2 })

	// The controller is cut off while more Variables changes happen than
	// the event buffer holds: on reconnect the server answers 410 Gone and
	// the informer lists again, without losing a change.
	backend.cut()
	for i := range 12 {
		putWidgetAPI(t, client, fmt.Sprintf("b%d", i), `{"size":1}`)
	}
	_, err = client.Variables().Delete(widgetKind.Path("b0"), nil)
	must.NoError(t, err)
	backend.restore()
	waitForLong(t, "resynchronized", func() bool { return len(c.Informer().List()) == 12 })
	must.Greater(t, 0, metrics.Value("norca_controller_resync_total",
		"controller", "widgets", "source", "objects", "reason", "gone"))
	waitForLong(t, "jobs of new objects", func() bool { return jobCountAPI(client, "widget-b11") == 1 })
	must.Eq(t, -1, jobCountAPI(client, "widget-b0"))

	// Deleting an object deletes its job.
	_, err = client.Variables().Delete(widgetKind.Path("a"), nil)
	must.NoError(t, err)
	waitForLong(t, "job deleted", func() bool {
		_, _, err := client.Jobs().Info("widget-a", nil)
		return err != nil
	})
}

func TestIntegration_LeaderElection(t *testing.T) {
	client := startAgent(t, 100)
	backend := controller.NewBackend(client)
	a := startCandidate(t, backend, "a")
	waitForLong(t, "a leads", a.leading.Load)
	b := startCandidate(t, backend, "b")
	time.Sleep(1500 * time.Millisecond)
	must.False(t, b.leading.Load())
	a.stop()
	waitForLong(t, "b leads", b.leading.Load)
}

func TestIntegration_StatusConflict(t *testing.T) {
	client := startAgent(t, 100)
	backend := controller.NewBackend(client)
	objects, err := controller.NewObjects(widgetKind, backend)
	must.NoError(t, err)
	ctx := context.Background()

	created, err := objects.Create(ctx, &controller.Object{Key: controller.ObjectKey{Namespace: "default", Name: "c"}, Spec: []byte(`{"size":1}`)})
	must.NoError(t, err)
	_, err = objects.Create(ctx, created)
	must.ErrorIs(t, err, controller.ErrConflict)

	// A concurrent spec change: the stale update gets 409, the status
	// update re-reads and keeps the new spec.
	newer := created.Copy()
	newer.Spec = []byte(`{"size":9}`)
	_, err = objects.Update(ctx, newer)
	must.NoError(t, err)
	_, err = objects.Update(ctx, created)
	must.ErrorIs(t, err, controller.ErrConflict)

	out, err := objects.UpdateStatus(ctx, created, widgetStatus{Size: 1})
	must.NoError(t, err)
	must.Eq(t, `{"size":9}`, string(out.Spec))

	_, err = backend.ReadVariable(ctx, "default", widgetKind.Path("missing"))
	must.ErrorIs(t, err, controller.ErrNotFound)
}

func waitForLong(t *testing.T, desc string, fn func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(fn),
		wait.Timeout(30*time.Second),
		wait.Gap(100*time.Millisecond),
	), must.Sprint(desc))
}
