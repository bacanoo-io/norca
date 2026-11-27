// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/helper/telemetry/otlptest"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/version"
	"github.com/hashicorp/go-hclog"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestOTLPIdentity(t *testing.T) {
	ci.Parallel(t)

	s := NewTestAgent(t, t.Name(), nil)
	defer s.Shutdown()

	id := otlpIdentity(s.Config, s.Agent)
	must.Eq(t, version.NorcaVersion, id.Version)
	must.Eq(t, s.Config.Region, id.Region)
	must.Eq(t, s.Config.Datacenter, id.Datacenter)
	must.Eq(t, s.Agent.Client().NodeID(), id.NodeID)
	must.Eq(t, s.Agent.Client().Node().Name, id.NodeName)
	must.Eq(t, s.Agent.Client().Node().NodePool, id.NodePool)
	must.Eq(t, s.Agent.Server().GetConfig().NodeID, id.ServerID)
	must.NotEq(t, "", id.HostName)

	s.Config.Telemetry.UseNodeName = true
	must.Eq(t, s.Config.NodeName, otlpIdentity(s.Config, s.Agent).HostName)

	// before the agent runs: what the configuration tells
	before := otlpIdentity(s.Config, nil)
	must.Eq(t, telemetry.Identity{
		Version:    version.NorcaVersion,
		Region:     s.Config.Region,
		Datacenter: s.Config.Datacenter,
		HostName:   s.Config.NodeName,
	}, before)
}

// otlpAgentFile writes the configuration of a -dev agent on free ports with
// the given otlp block; Prometheus is off, so several agents can run in one
// test binary.
func otlpAgentFile(t *testing.T, otlp string) string {
	t.Helper()
	path, _ := otlpAgentFileHTTP(t, otlp)
	return path
}

// otlpAgentFileHTTP is otlpAgentFile that also returns the HTTP port.
func otlpAgentFileHTTP(t *testing.T, otlp string) (string, int) {
	t.Helper()
	ports := ci.PortAllocator.Grab(5)
	content := fmt.Sprintf(`
ports {
  http        = %d
  rpc         = %d
  serf        = %d
  bus         = %d
  bus_cluster = %d
}
server {
  bus { enabled = false }
}
telemetry {
  prometheus { enabled = false }
  %s
}
`, ports[0], ports[1], ports[2], ports[3], ports[4], otlp)
	path := filepath.Join(t.TempDir(), "agent.hcl")
	must.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path, ports[0]
}

// runAgent runs a -dev agent through Command.Run until stop is closed.
func runAgent(t *testing.T, configFile string, stop <-chan struct{}) (*cli.MockUi, <-chan int) {
	t.Helper()
	ui := cli.NewMockUi()
	cmd := &Command{Version: version.GetVersion(), Ui: ui, ShutdownCh: stop}
	done := make(chan int, 1)
	go func() { done <- cmd.Run([]string{"-dev", "-config=" + configFile}) }()
	return ui, done
}

func waitExit(t *testing.T, done <-chan int, within time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("the agent did not stop within %s", within)
	}
}

func protoAttrs(res *resourcepb.Resource) map[string]*commonpb.AnyValue {
	out := map[string]*commonpb.AnyValue{}
	for _, kv := range res.GetAttributes() {
		out[kv.GetKey()] = kv.GetValue()
	}
	return out
}

// A running agent exports to the receiver of its otlp block, with the IDs of
// the client and the server in the resource, its metrics named norca.*,
// Prometheus off. Not parallel: Command.Run sets
// process-wide state (the standard logger, the go-metrics global).
func TestCommand_OTLP_Export(t *testing.T) {
	srv := otlptest.Start(t, otlptest.Options{})
	file := otlpAgentFile(t, fmt.Sprintf(`otlp {
    endpoint = %q
    headers  = { "x-tenant" = "team-a" }
    resource_attributes = { "deployment.environment.name" = "test" }
    metrics { interval = "500ms" }
  }`, srv.Addr))
	stop := make(chan struct{})
	ui, done := runAgent(t, file, stop)

	withIDs := func(s *otlptest.Server) bool {
		for _, m := range s.Metrics() {
			attrs := protoAttrs(m.Request.GetResourceMetrics()[0].GetResource())
			if attrs["norca.node.id"].GetStringValue() != "" {
				return true
			}
		}
		return false
	}
	ok := srv.Wait(60*time.Second, withIDs)
	if !ok {
		close(stop)
		waitExit(t, done, 30*time.Second)
		t.Fatalf("no metrics export with the resource of the agent:\n%s\n%s", ui.OutputWriter.String(), ui.ErrorWriter.String())
	}

	m := srv.Metrics()[0]
	must.Eq(t, "team-a", m.Header("x-tenant"))
	must.StrContains(t, m.Header("user-agent"), "Norca/"+version.NorcaVersion)
	attrs := protoAttrs(m.Request.GetResourceMetrics()[0].GetResource())
	must.Eq(t, "norca", attrs["service.name"].GetStringValue())
	must.Eq(t, version.NorcaVersion, attrs["service.version"].GetStringValue())
	must.Eq(t, "global", attrs["norca.region"].GetStringValue())
	must.Eq(t, "dc1", attrs["norca.datacenter"].GetStringValue())
	must.Eq(t, "test", attrs["deployment.environment.name"].GetStringValue())
	must.NotEq(t, "", attrs["norca.server.id"].GetStringValue())
	nodeID := attrs["norca.node.id"].GetStringValue()
	must.Eq(t, nodeID, attrs["service.instance.id"].GetStringValue())
	var roles []string
	for _, v := range attrs["norca.agent.roles"].GetArrayValue().GetValues() {
		roles = append(roles, v.GetStringValue())
	}
	must.Eq(t, []string{"server", "client"}, roles)

	// The go-metrics of the agent arrive through the bridge with their
	// norca.* names: the server's RPCs, Raft and the Go runtime.
	families := []string{"norca.server.rpc.", "norca.raft.", "norca.runtime.", "norca.client.", "norca.memberlist.gossip"}
	allFamilies := func(s *otlptest.Server) bool {
		names := exportedMetrics(s)
		for _, family := range families {
			if !slices.ContainsFunc(slices.Collect(maps.Keys(names)), func(n string) bool { return strings.HasPrefix(n, family) }) {
				return false
			}
		}
		return true
	}
	ok = srv.Wait(60*time.Second, allFamilies)
	names := exportedMetrics(srv)
	close(stop)
	waitExit(t, done, 30*time.Second)
	must.True(t, ok, must.Sprintf("not every family of %v was exported: %v", families, slices.Sorted(maps.Keys(names))))
	t.Logf("%d metrics exported: %v", len(names), slices.Sorted(maps.Keys(names)))
	for name := range names {
		must.StrHasPrefix(t, "norca.", name)
	}
	must.Eq(t, "ms", names["norca.memberlist.gossip"], must.Sprint("timers are histograms in milliseconds"))
}

// What the agent does before it has its identity (the providers get it once
// the agent is set up) is traced and exported with the complete resource:
// the hook holds the identity back until the leader processed the
// evaluation of a job registered meanwhile, as a restarted server does with
// the evaluations it enqueues again (decision 24 of the plan). Not
// parallel: Command.Run sets process-wide state.
func TestCommand_OTLP_StartWindow(t *testing.T) {
	srv := otlptest.Start(t, otlptest.Options{})
	file := otlpAgentFile(t, fmt.Sprintf(`otlp {
    endpoint = %q
    metrics { enabled = false }
    traces  { sample_ratio = 1.0 }
  }`, srv.Addr))

	type hookResult struct {
		evalID string
		err    error
	}
	hooked := make(chan hookResult, 1)
	ui := cli.NewMockUi()
	stop := make(chan struct{})
	cmd := &Command{Version: version.GetVersion(), Ui: ui, ShutdownCh: stop}
	cmd.agentSetUp = func(a *Agent) {
		id, err := processEval(a)
		hooked <- hookResult{evalID: id, err: err}
	}
	done := make(chan int, 1)
	go func() { done <- cmd.Run([]string{"-dev", "-config=" + file}) }()
	exited := false
	defer func() {
		if !exited {
			close(stop)
			waitExit(t, done, 30*time.Second)
		}
	}()

	var res hookResult
	select {
	case res = <-hooked:
	case code := <-done:
		exited = true
		t.Fatalf("the agent exited with %d before its setup was done:\n%s", code, ui.ErrorWriter.String())
	case <-time.After(90 * time.Second):
		t.Fatal("the agent's setup did not end")
	}
	must.NoError(t, res.err)

	// the eval.process span of the evaluation, with the agent's IDs
	evalSpan := func(s *otlptest.Server) *resourcepb.Resource {
		for _, req := range s.Traces() {
			for _, rs := range req.Request.GetResourceSpans() {
				for _, ss := range rs.GetScopeSpans() {
					for _, span := range ss.GetSpans() {
						if span.GetName() != telemetry.SpanEvalProcess {
							continue
						}
						for _, kv := range span.GetAttributes() {
							if kv.GetKey() == string(telemetry.AttrEvalID) && kv.GetValue().GetStringValue() == res.evalID {
								return rs.GetResource()
							}
						}
					}
				}
			}
		}
		return nil
	}
	ok := srv.Wait(30*time.Second, func(s *otlptest.Server) bool { return evalSpan(s) != nil })
	must.True(t, ok, must.Sprintf("no eval.process span of evaluation %s", res.evalID))
	attrs := protoAttrs(evalSpan(srv))
	nodeID := attrs["norca.node.id"].GetStringValue()
	must.NotEq(t, "", nodeID)
	must.Eq(t, nodeID, attrs["service.instance.id"].GetStringValue())
	must.NotEq(t, "", attrs["norca.server.id"].GetStringValue())
	must.Eq(t, "global", attrs["norca.region"].GetStringValue())
}

// processEval registers a job on the agent's server once it leads and waits
// until a worker processed its evaluation, whose ID it returns.
func processEval(a *Agent) (string, error) {
	server := a.Server()
	deadline := time.Now().Add(60 * time.Second)
	for !server.IsLeader() {
		if time.Now().After(deadline) {
			return "", errors.New("the server did not become the leader")
		}
		time.Sleep(50 * time.Millisecond)
	}
	job := mock.Job()
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: a.config.Region, Namespace: job.Namespace},
	}
	var resp structs.JobRegisterResponse
	if err := a.RPC("Job.Register", req, &resp); err != nil {
		return "", fmt.Errorf("registering the job: %w", err)
	}
	for {
		eval, err := server.State().EvalByID(nil, resp.EvalID)
		if err != nil {
			return resp.EvalID, err
		}
		if eval != nil && eval.Status != structs.EvalStatusPending {
			return resp.EvalID, nil
		}
		if time.Now().After(deadline) {
			return resp.EvalID, errors.New("the evaluation was not processed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// exportedMetrics returns the names of the metrics the receiver got, with
// their units.
func exportedMetrics(s *otlptest.Server) map[string]string {
	out := map[string]string{}
	for _, m := range s.Metrics() {
		for _, rm := range m.Request.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, metric := range sm.GetMetrics() {
					out[metric.GetName()] = metric.GetUnit()
				}
			}
		}
	}
	return out
}

// With the receiver absent the agent starts as usual, logs one warning per
// signal per minute and shuts down within the deadline of the export.
func TestCommand_OTLP_ClosedPort(t *testing.T) {
	file := otlpAgentFile(t, fmt.Sprintf(`otlp {
    endpoint = %q
    timeout  = "1s"
    retry { enabled = false }
    metrics { interval = "300ms" }
  }`, otlptest.ClosedAddr(t)))
	stop := make(chan struct{})
	start := time.Now()
	ui, done := runAgent(t, file, stop)

	started := func() bool { return strings.Contains(ui.OutputWriter.String(), "Norca agent started!") }
	deadline := time.Now().Add(60 * time.Second)
	for !started() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	must.True(t, started(), must.Sprintf("the agent did not start:\n%s", ui.ErrorWriter.String()))
	t.Logf("agent started after %s", time.Since(start))

	// several export intervals pass
	time.Sleep(3 * time.Second)

	close(stop)
	stopped := time.Now()
	waitExit(t, done, 30*time.Second)
	t.Logf("agent stopped after %s", time.Since(stopped))

	out := ui.OutputWriter.String()
	must.Eq(t, 1, strings.Count(out, "OTLP export failed"), must.Sprint(out))
	must.StrContains(t, out, "signal=metrics")
}

// The log lines of a running agent arrive as log records: severity, the
// logger, typed attributes, and the trace context of the HTTP request they
// were written under, the same as of its spans. Not parallel: Command.Run
// sets process-wide state.
func TestCommand_OTLP_Logs(t *testing.T) {
	srv := otlptest.Start(t, otlptest.Options{})
	file, httpPort := otlpAgentFileHTTP(t, fmt.Sprintf(`otlp {
    endpoint = %q
    metrics { enabled = false }
    traces  { sample_ratio = 1.0 }
    logs    {
      enabled = true
      level   = "debug"
    }
  }`, srv.Addr))
	stop := make(chan struct{})
	ui, done := runAgent(t, file, stop)
	stopped := false
	stopAgent := func() {
		if !stopped {
			stopped = true
			close(stop)
			waitExit(t, done, 30*time.Second)
		}
	}
	defer stopAgent()

	// a request through the HTTP API; the response names its trace
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/jobs", httpPort)
	var traceID string
	deadline := time.Now().Add(60 * time.Second)
	for traceID == "" && time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				traceID = resp.Header.Get(telemetry.TraceIDHeader)
			}
		}
		if traceID == "" {
			time.Sleep(200 * time.Millisecond)
		}
	}
	must.NotEq(t, "", traceID, must.Sprintf("no traced response from %s:\n%s", url, ui.ErrorWriter.String()))

	requestLog := func(s *otlptest.Server) *logspb.LogRecord {
		for _, req := range s.Logs() {
			for _, rl := range req.Request.GetResourceLogs() {
				for _, sl := range rl.GetScopeLogs() {
					for _, lr := range sl.GetLogRecords() {
						if lr.GetBody().GetStringValue() == "request complete" && hex.EncodeToString(lr.GetTraceId()) == traceID {
							return lr
						}
					}
				}
			}
		}
		return nil
	}
	ok := srv.Wait(30*time.Second, func(s *otlptest.Server) bool { return requestLog(s) != nil })
	must.True(t, ok, must.Sprint("the log line of the request was not exported"))
	lr := requestLog(srv)
	must.Eq(t, logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, lr.GetSeverityNumber())
	attrs := map[string]*commonpb.AnyValue{}
	for _, kv := range lr.GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue()
	}
	must.Eq(t, "http", attrs[telemetry.AttrLogger].GetStringValue())
	must.Eq(t, "GET", attrs["http_method"].GetStringValue())
	must.StrHasPrefix(t, "/v1/jobs", attrs["http_path"].GetStringValue())
	_, isFloat := attrs["duration"].GetValue().(*commonpb.AnyValue_DoubleValue)
	must.True(t, isFloat, must.Sprint("the duration is a number of milliseconds"))

	// the span of the request has the same trace ID
	ok = srv.Wait(30*time.Second, func(s *otlptest.Server) bool {
		for _, req := range s.Traces() {
			for _, rs := range req.Request.GetResourceSpans() {
				for _, ss := range rs.GetScopeSpans() {
					for _, span := range ss.GetSpans() {
						if hex.EncodeToString(span.GetTraceId()) == traceID {
							return true
						}
					}
				}
			}
		}
		return false
	})
	must.True(t, ok, must.Sprint("no span of the request's trace"))
	stopAgent()

	// the shutdown lines went out too: the logs are flushed last
	var shutdown bool
	for _, req := range srv.Logs() {
		for _, rl := range req.Request.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					shutdown = shutdown || strings.Contains(lr.GetBody().GetStringValue(), "shutdown")
				}
			}
		}
	}
	must.True(t, shutdown, must.Sprint("no line of the shutdown was exported"))
}

// recordingLogger counts the sinks registered on it.
type recordingLogger struct {
	hclog.InterceptLogger
	sinks []hclog.SinkAdapter
}

func (r *recordingLogger) RegisterSink(sink hclog.SinkAdapter) {
	r.sinks = append(r.sinks, sink)
}

// Without the logs signal no sink is registered on the agent's logger.
func TestRegisterOTLPLogSink(t *testing.T) {
	ci.Parallel(t)

	newProviders := func(c *telemetry.OTLPConfig) *telemetry.Providers {
		s, err := telemetry.Resolve(c)
		must.NoError(t, err)
		p, err := telemetry.New(s, hclog.NewNullLogger(), "norca-test/1")
		must.NoError(t, err)
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		return p
	}
	base := hclog.NewInterceptLogger(&hclog.LoggerOptions{Output: io.Discard})

	logger := &recordingLogger{InterceptLogger: base}
	registerOTLPLogSink(logger, nil)
	must.SliceEmpty(t, logger.sinks, must.Sprint("no otlp block"))

	registerOTLPLogSink(logger, newProviders(&telemetry.OTLPConfig{}))
	must.SliceEmpty(t, logger.sinks, must.Sprint("logs.enabled defaults to false"))

	registerOTLPLogSink(logger, newProviders(&telemetry.OTLPConfig{
		Logs: &telemetry.LogsConfig{Enabled: pointer.Of(false)},
	}))
	must.SliceEmpty(t, logger.sinks, must.Sprint("logs.enabled = false"))

	p := newProviders(&telemetry.OTLPConfig{Logs: &telemetry.LogsConfig{Enabled: pointer.Of(true)}})
	registerOTLPLogSink(logger, p)
	must.Len(t, 1, logger.sinks)
	must.Eq(t, hclog.SinkAdapter(p.LogSink()), logger.sinks[0])
}
