// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metrics "github.com/armon/go-metrics"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// statsCallSink is a metrics sink that records whether any allocation stats
// RPC (server side ClientAllocations.Stats or client side Allocations.Stats)
// was measured. It also counts Node.UpdateAlloc calls, to show that it sees
// the RPC measurements at all.
type statsCallSink struct {
	metrics.BlackholeSink

	l            sync.Mutex
	calls        []string
	updateAllocs int
}

func (s *statsCallSink) record(key []string) {
	k := strings.Join(key, ".")
	s.l.Lock()
	defer s.l.Unlock()
	switch {
	case strings.Contains(k, "allocations.stats"):
		s.calls = append(s.calls, k)
	case strings.HasSuffix(k, "client.update_alloc"):
		s.updateAllocs++
	}
}

func (s *statsCallSink) numUpdateAllocs() int {
	s.l.Lock()
	defer s.l.Unlock()
	return s.updateAllocs
}

func (s *statsCallSink) AddSample(key []string, _ float32) { s.record(key) }

func (s *statsCallSink) AddSampleWithLabels(key []string, _ float32, _ []metrics.Label) {
	s.record(key)
}

func (s *statsCallSink) statsCalls() []string {
	s.l.Lock()
	defer s.l.Unlock()
	return append([]string(nil), s.calls...)
}

// TestAutoscaler_BuiltinMetricFromClients runs a server with the built-in
// autoscaler and a client with the mock driver. A policy on nomad.cpu is
// evaluated from the usage the client publishes on the system bus, without
// any allocation stats RPC.
func TestAutoscaler_BuiltinMetricFromClients(t *testing.T) {
	// Not parallel: the test replaces the global metrics sink.
	sink := &statsCallSink{}
	metricsConf := metrics.DefaultConfig("")
	metricsConf.EnableHostname = false
	metricsConf.EnableRuntimeMetrics = false
	_, err := metrics.NewGlobal(metricsConf, sink)
	must.NoError(t, err)
	t.Cleanup(metrics.Shutdown)

	const window = 3 * time.Second
	s, cleanupS := TestServer(t, func(c *Config) {
		enableBus(t)(c)
		c.Autoscaler = autoscaler.DefaultConfig()
		c.Autoscaler.Enabled = true
		c.Autoscaler.DefaultEngine = structs.ScalingPolicyEngineBuiltin
		c.Autoscaler.MinEvalInterval = time.Second
		c.Autoscaler.MinCooldown = time.Second
		c.Autoscaler.DefaultEvaluationInterval = time.Second
		c.Autoscaler.MetricsWindow = window
		c.Autoscaler.ClientMetricsInterval = time.Second
	})
	defer cleanupS()
	testutil.WaitForLeader(t, s.RPC)

	c, cleanupC := client.TestClient(t, func(c *config.Config) {
		c.Servers = []string{s.config.RPCAddr.String()}
		c.StatsCollectionInterval = 200 * time.Millisecond
	})
	defer cleanupC()

	testutil.WaitForClient(t, s.RPC, c.NodeID(), s.Region())

	// The mock driver reports no CPU usage, so nomad.cpu is 0% and the
	// target-value strategy scales the group down to its minimum.
	job := mock.MinJob()
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Scaling = &structs.ScalingPolicy{
		Min:     1,
		Max:     5,
		Enabled: true,
		Type:    structs.ScalingPolicyTypeHorizontal,
		Policy: map[string]interface{}{
			"evaluation_interval": "1s",
			"cooldown":            "1s",
			"check": []interface{}{
				map[string]interface{}{
					"cpu": []interface{}{
						map[string]interface{}{
							"metric": autoscaler.MetricCPU,
							"strategy": []interface{}{
								map[string]interface{}{
									"target-value": []interface{}{
										map[string]interface{}{"target": 50},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// The HTTP API sets the target; the RPC does not.
	job.TaskGroups[0].Scaling.TargetTaskGroup(job, job.TaskGroups[0])

	codec := rpcClient(t, s)
	regReq := &structs.JobRegisterRequest{
		Job: job,
		WriteRequest: structs.WriteRequest{
			Region:    "global",
			Namespace: job.Namespace,
		},
	}
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", regReq, &regResp))

	// The heartbeat asks the client for task group usage, and the client
	// reports the usage of the running allocations to the
	// leader's collector.
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return s.autoscalerUsageGroupsForTest() == 1 }),
		wait.Timeout(30*time.Second),
		wait.Gap(200*time.Millisecond),
	))

	// After one full metric window the policy scales the group down.
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			stored, err := s.State().JobByID(nil, job.Namespace, job.ID)
			if err != nil {
				return err
			}
			if n := stored.TaskGroups[0].Count; n != 1 {
				return fmt.Errorf("task group count is %d", n)
			}
			return nil
		}),
		wait.Timeout(60*time.Second),
		wait.Gap(250*time.Millisecond),
	))

	events, _, err := s.State().ScalingEventsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.SliceNotEmpty(t, events[job.TaskGroups[0].Name])
	must.Eq(t, structs.ScalingPolicyEngineBuiltin,
		events[job.TaskGroups[0].Name][0].Meta["nomad_autoscaler.engine"])

	must.Positive(t, sink.numUpdateAllocs())
	must.SliceEmpty(t, sink.statsCalls())
}

// TestBusTelemetry_TaskGroupUsage checks the heartbeat setting and the usage
// reports on the system bus: kept by the leader's collector, and only from
// the node of the subject.
func TestBusTelemetry_TaskGroupUsage(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, func(c *Config) {
		enableBus(t)(c)
		c.Autoscaler = autoscaler.DefaultConfig()
		c.Autoscaler.Enabled = true
		c.Autoscaler.ClientMetricsInterval = 5 * time.Second
	})
	defer cleanupS()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	node := mock.Node()
	reg := &structs.NodeRegisterRequest{
		Node:         node,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var regResp structs.NodeUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.Register", reg, &regResp))
	must.Eq(t, 5*time.Second, regResp.TaskGroupUsageInterval)

	// The leader creates the telemetry stream.
	js := busJS(t, s)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), bus.TelemetryStreamName("global"))
		return err == nil, err
	})

	nc, err := nats.Connect(busURL(s), nats.UserInfo(node.ID, node.SecretID))
	must.NoError(t, err)
	defer nc.Close()
	publish := func(report *structs.TaskGroupUsageReport) {
		b, err := json.Marshal(report)
		must.NoError(t, err)
		must.NoError(t, nc.Publish(structs.BusTelemetrySubject("global", node.ID), b))
		must.NoError(t, nc.Flush())
	}
	group := func(job string) []*structs.TaskGroupUsage {
		return []*structs.TaskGroupUsage{{
			Namespace: structs.DefaultNamespace, JobID: job, TaskGroup: "web", Allocs: 1, Samples: 5,
		}}
	}

	// A report naming another node on the subject of this one is ignored.
	publish(&structs.TaskGroupUsageReport{NodeID: uuid.Generate(), Window: 5 * time.Second, Groups: group("spoofed")})

	// The leader's collector keeps the usage of the node.
	publish(&structs.TaskGroupUsageReport{NodeID: node.ID, Window: 5 * time.Second, Groups: group("api")})
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return s.autoscalerUsageGroupsForTest() == 1 }),
		wait.Timeout(30*time.Second),
		wait.Gap(100*time.Millisecond),
	))

	// With the autoscaler disabled clients are not asked for usage.
	cfg := autoscaler.DefaultConfig()
	s.autoscaler.SetConfig(cfg)
	hb := &structs.NodeUpdateStatusRequest{
		NodeID:       node.ID,
		Status:       structs.NodeStatusReady,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: node.SecretID},
	}
	var hbResp structs.NodeUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.UpdateStatus", hb, &hbResp))
	must.Eq(t, time.Duration(0), hbResp.TaskGroupUsageInterval)
}

// TestBusTelemetry_NoBus checks that without the system bus the clients are
// not asked for usage: the reports travel on the bus only.
func TestBusTelemetry_NoBus(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, func(c *Config) {
		c.Autoscaler = autoscaler.DefaultConfig()
		c.Autoscaler.Enabled = true
		c.Autoscaler.ClientMetricsInterval = 5 * time.Second
	})
	defer cleanupS()
	testutil.WaitForLeader(t, s.RPC)

	var regResp structs.NodeUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), "Node.Register", &structs.NodeRegisterRequest{
		Node:         mock.Node(),
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &regResp))
	must.Eq(t, time.Duration(0), regResp.TaskGroupUsageInterval)
}

// autoscalerUsageGroupsForTest returns the number of task groups in the
// leader's usage collector.
func (s *Server) autoscalerUsageGroupsForTest() int {
	return s.autoscaler.UsageGroups()
}
