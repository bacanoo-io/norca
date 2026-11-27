// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry/otlptest"
	"github.com/bacanoo-io/norca/norca/version"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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
}

// otlpAgentFile writes the configuration of a -dev agent on free ports with
// the given otlp block; Prometheus is off, so several agents can run in one
// test binary.
func otlpAgentFile(t *testing.T, otlp string) string {
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
	return path
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
