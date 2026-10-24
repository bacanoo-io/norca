// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// The integration test runs against a real dev agent started from the
// binary named by NORCA_BINARY, or "norca" (or "nomad") on the PATH. The
// module cannot import the main module's in-process test server, so the
// agent runs as a separate process. It is skipped with -short or when no
// binary is found.

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
// by the time the agent binds it.
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

// registerPending registers a job that is valid but cannot be placed, so no
// workload runs.
func registerPending(t *testing.T, client *api.Client, id string) {
	t.Helper()
	job := api.NewServiceJob(id, id, "global", 50)
	job.Datacenters = []string{"*"}
	job.Constraints = []*api.Constraint{api.NewConstraint("${attr.kernel.name}", "=", "no-such-kernel")}
	tg := api.NewTaskGroup("g", 1)
	tg.Tasks = []*api.Task{api.NewTask("t", "raw_exec").SetConfig("command", "unused")}
	job.TaskGroups = []*api.TaskGroup{tg}
	_, _, err := client.Jobs().Register(job, nil)
	must.NoError(t, err)
}

func waitLong(t *testing.T, what string, f func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(f),
		wait.Timeout(60*time.Second),
		wait.Gap(100*time.Millisecond),
	), must.Sprint(what))
}

// streamMessages returns the messages stored in a JetStream stream.
func streamMessages(t *testing.T, js jetstream.JetStream, name string) []*jetstream.RawStreamMsg {
	t.Helper()
	ctx := context.Background()
	stream, err := js.Stream(ctx, name)
	must.NoError(t, err)
	info, err := stream.Info(ctx)
	must.NoError(t, err)
	var out []*jetstream.RawStreamMsg
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		msg, err := stream.GetMsg(ctx, seq)
		if err == nil {
			out = append(out, msg)
		}
	}
	return out
}

func countMsgs(msgs []*jetstream.RawStreamMsg, topic, typ, key string) int {
	n := 0
	for _, m := range msgs {
		if m.Header.Get(HeaderTopic) == topic && m.Header.Get(HeaderType) == typ && m.Header.Get(HeaderKey) == key {
			n++
		}
	}
	return n
}

func checkpointIndex(t *testing.T, client *api.Client, path string) uint64 {
	v, _, err := client.Variables().Peek(path, nil)
	must.NoError(t, err)
	if v == nil {
		return 0
	}
	i, _ := strconv.ParseUint(v.Items["index"], 10, 64)
	return i
}

func TestIntegration_Bridge(t *testing.T) {
	client := startAgent(t, 8)
	s := runNATS(t, true)
	nc := connect(t, s)
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	sub := collect(t, nc, "nomad.global.>")

	metricsAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg, err := ParseConfig([]string{
		"-addr", client.Address(),
		"-nats-url", s.ClientURL(),
		"-jetstream", "-jetstream-stream", "NOMAD", "-jetstream-create",
		"-topics", "Job,Evaluation",
		"-state-prefix", "natsbridge/test",
		"-lease-duration", "3s",
		"-checkpoint-interval", "100ms",
		"-metrics-addr", metricsAddr,
	}, func(string) string { return "" }, io.Discard)
	must.NoError(t, err)

	run := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, cfg, testLogger(t)) }()
		return func() {
			cancel()
			must.NoError(t, <-done)
		}
	}

	// Seed the checkpoint with the current index, so the first run does
	// not depend on when its subscription starts.
	seed := api.NewVariable("natsbridge/test/checkpoint")
	seed.Items["index"] = "1"
	seed, _, err = client.Variables().Create(seed, nil)
	must.NoError(t, err)
	seed.Items["index"] = strconv.FormatUint(seed.ModifyIndex, 10)
	_, _, err = client.Variables().Update(seed, nil)
	must.NoError(t, err)

	stop := run()
	registerPending(t, client, "a")
	waitLong(t, "JobRegistered of a", func() bool {
		for _, m := range sub.all() {
			if m.Subject == "nomad.global.Job" && m.Header.Get(HeaderKey) == "a" && m.Header.Get(HeaderType) == "JobRegistered" {
				return true
			}
		}
		return false
	})
	var msgA struct {
		Region  string
		Topic   string
		Index   uint64
		Payload struct{ Job struct{ ID string } }
	}
	for _, m := range sub.all() {
		if m.Header.Get(HeaderKey) == "a" && m.Header.Get(HeaderType) == "JobRegistered" {
			must.NoError(t, json.Unmarshal(m.Data, &msgA))
			must.Eq(t, fmt.Sprintf("global:%d:Job:JobRegistered:default:a", msgA.Index), m.Header.Get(HeaderMsgID))
		}
	}
	must.Eq(t, "global", msgA.Region)
	must.Eq(t, "a", msgA.Payload.Job.ID)
	waitLong(t, "evaluation event", func() bool {
		for _, m := range sub.all() {
			if m.Subject == "nomad.global.Evaluation" {
				return true
			}
		}
		return false
	})
	waitLong(t, "checkpoint", func() bool { return checkpointIndex(t, client, "natsbridge/test/checkpoint") >= msgA.Index })

	resp, err := http.Get("http://" + metricsAddr + "/metrics")
	must.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	must.StrContains(t, string(body), `norca_natsbridge_events_published_total{topic="Job"}`)
	must.StrContains(t, string(body), "norca_natsbridge_leader 1")

	lease, _, err := client.Variables().Read("natsbridge/test/lease", nil)
	must.NoError(t, err)
	must.NotEq(t, "", lease.Items["holder"])
	stop()

	// Stopped: the lease is released.
	lease, _, err = client.Variables().Read("natsbridge/test/lease", nil)
	must.NoError(t, err)
	must.Eq(t, "", lease.Items["holder"])

	// Events while stopped are published after the restart, the events
	// before the checkpoint are not published again.
	registerPending(t, client, "b")
	stop = run()
	waitLong(t, "JobRegistered of b", func() bool {
		return countMsgs(streamMessages(t, js, "NOMAD"), "Job", "JobRegistered", "b") == 1
	})
	stop()
	msgs := streamMessages(t, js, "NOMAD")
	must.Eq(t, 1, countMsgs(msgs, "Job", "JobRegistered", "a"))
	must.Eq(t, 0, countMsgs(msgs, GapTopic, GapType, "global"))

	// More events while stopped than the event buffer holds: the server
	// answers 410 Gone, the bridge publishes a gap marker and continues
	// with the newest events.
	for i := range 12 {
		registerPending(t, client, fmt.Sprintf("c%d", i))
	}
	stop = run()
	defer stop()
	waitLong(t, "gap marker", func() bool {
		return countMsgs(streamMessages(t, js, "NOMAD"), GapTopic, GapType, "global") == 1
	})
	registerPending(t, client, "d")
	waitLong(t, "JobRegistered of d", func() bool {
		return countMsgs(streamMessages(t, js, "NOMAD"), "Job", "JobRegistered", "d") == 1
	})
}
