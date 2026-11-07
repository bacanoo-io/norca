// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"io"
	"net/rpc"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	metrics "github.com/hashicorp/go-metrics"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// TestMetricsStore_ServerGauges checks the gauges of the internals of a
// server from the values it read: the Raft statistics as numbers, the last
// contact on the leader and on a server that never heard from one, and no
// point for what is not known.
func TestMetricsStore_ServerGauges(t *testing.T) {
	ci.Parallel(t)

	values := serverValues{
		raftDBBytes: 1 << 20,
		raftDBKnown: true,
		raft: map[string]string{
			"last_contact":  "1.5s",
			"commit_index":  "120",
			"applied_index": "117",
			"fsm_pending":   "2",
			"term":          "4",
		},
		bus: &bus.Sample{JetStream: true, Bytes: 4096, Messages: 12, RouteServers: 2},
		timings: raftTimings{
			commit:        0.004,
			commitKnown:   true,
			appendEntries: map[string]float64{"server-2": 0.002, "server-3": 0.003},
		},
		rpcRequests: 50,
		rpcErrors:   2,
	}
	var counters counterSampler
	must.Eq(t, map[string]float64{
		metricsstore.MetricServerRaftDBBytes:                     1 << 20,
		metricsstore.MetricServerRaftLastContact:                 1.5,
		metricsstore.MetricServerRaftCommitLag:                   3,
		metricsstore.MetricServerRaftFSMPending:                  2,
		metricsstore.MetricServerRaftTerm:                        4,
		metricsstore.MetricServerRaftCommit:                      0.004,
		metricsstore.MetricServerRaftAppendEntries + "/server-2": 0.002,
		metricsstore.MetricServerRaftAppendEntries + "/server-3": 0.003,
		metricsstore.MetricServerRPCRequests:                     50,
		metricsstore.MetricServerRPCErrors:                       2,
		metricsstore.MetricBusJetStreamBytes:                     4096,
		metricsstore.MetricBusJetStreamMessages:                  12,
		metricsstore.MetricBusRoutes:                             2,
	}, gaugeValues(serverGauges(values, &counters)))

	// The leader has no delay; a server that never heard from a leader has
	// no point. An applied index ahead of the commit index (after a restart)
	// is no lag. The RPC counters are their increase since the last sample.
	values.raft["last_contact"] = "0"
	values.raft["applied_index"] = "130"
	values.rpcRequests = 80
	got := gaugeValues(serverGauges(values, &counters))
	must.Eq(t, 0.0, got[metricsstore.MetricServerRaftLastContact])
	must.MapContainsKey(t, got, metricsstore.MetricServerRaftLastContact)
	must.Eq(t, 0.0, got[metricsstore.MetricServerRaftCommitLag])
	must.Eq(t, 30.0, got[metricsstore.MetricServerRPCRequests])
	must.Eq(t, 0.0, got[metricsstore.MetricServerRPCErrors])
	values.raft["last_contact"] = "never"
	got = gaugeValues(serverGauges(values, &counters))
	must.MapNotContainsKey(t, got, metricsstore.MetricServerRaftLastContact)

	// No size of the raft store, no JetStream, no Raft, no Raft times (a
	// follower): no points for them. The RPC counters always have one.
	values = serverValues{bus: &bus.Sample{RouteServers: 1}}
	must.Eq(t, map[string]float64{
		metricsstore.MetricBusRoutes:         1,
		metricsstore.MetricServerRPCRequests: 0,
		metricsstore.MetricServerRPCErrors:   0,
	}, gaugeValues(serverGauges(values, &counterSampler{})))
	must.Eq(t, map[string]float64{
		metricsstore.MetricServerRPCRequests: 0,
		metricsstore.MetricServerRPCErrors:   0,
	}, gaugeValues(serverGauges(serverValues{}, &counterSampler{})))
}

// alignTelemetryInterval waits until the current interval of a telemetry
// sink of the interval has at least margin left, so that what a test emits
// next lands in one interval.
func alignTelemetryInterval(interval, margin time.Duration) {
	now := time.Now()
	if left := now.Truncate(interval).Add(interval).Sub(now); left < margin {
		time.Sleep(left + time.Millisecond)
	}
}

// TestMetricsStore_RaftTimings checks that the Raft times are read from the
// telemetry sink: the mean of the commit time and of the AppendEntries time
// per follower in seconds, not the name with the peer in it nor other Raft
// times, each interval once, and nothing without a sample or a sink.
func TestMetricsStore_RaftTimings(t *testing.T) {
	ci.Parallel(t)

	var reader telemetryReader
	t1 := reader.raftTimings(nil)
	must.False(t, t1.commitKnown)
	must.MapEmpty(t, t1.appendEntries)

	const interval = 500 * time.Millisecond
	sink := metrics.NewInmemSink(interval, time.Minute)
	host := metrics.Label{Name: "host", Value: "server-1"}
	peer := func(id string) []metrics.Label {
		return []metrics.Label{{Name: "peer_id", Value: id}, host}
	}
	alignTelemetryInterval(interval, 200*time.Millisecond)
	sink.AddSampleWithLabels([]string{"nomad", "raft", "commitTime"}, 10, []metrics.Label{host})
	sink.AddSampleWithLabels([]string{"nomad", "raft", "commitTime"}, 30, []metrics.Label{host})
	sink.AddSampleWithLabels([]string{"nomad", "raft", "replication", "appendEntries", "rpc"}, 4, peer("server-2"))
	sink.AddSampleWithLabels([]string{"nomad", "raft", "replication", "appendEntries", "rpc"}, 6, peer("server-2"))
	sink.AddSampleWithLabels([]string{"nomad", "raft", "replication", "appendEntries", "rpc"}, 2, peer("server-3"))
	// The same time under the name with the peer, and the time of a
	// follower to handle an AppendEntries request: not read.
	sink.AddSampleWithLabels([]string{"nomad", "raft", "replication", "appendEntries", "rpc", "server-2"}, 500, []metrics.Label{host})
	sink.AddSampleWithLabels([]string{"nomad", "raft", "rpc", "appendEntries"}, 900, []metrics.Label{host})

	// Nothing until the interval is complete.
	var got raftTimings
	testutil.WaitForResult(func() (bool, error) {
		got = reader.raftTimings(sink)
		if !got.commitKnown {
			return false, fmt.Errorf("no commit time yet")
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })
	must.InDelta(t, 0.020, got.commit, 1e-9)
	must.MapLen(t, 2, got.appendEntries)
	must.InDelta(t, 0.005, got.appendEntries["server-2"], 1e-9)
	must.InDelta(t, 0.002, got.appendEntries["server-3"], 1e-9)

	// The interval was read: no point without a new sample.
	got = reader.raftTimings(sink)
	must.False(t, got.commitKnown)
	must.MapEmpty(t, got.appendEntries)

	// A follower has no commit time, only what the next interval brings.
	alignTelemetryInterval(interval, 200*time.Millisecond)
	sink.AddSampleWithLabels([]string{"nomad", "raft", "replication", "appendEntries", "rpc"}, 8, peer("server-2"))
	testutil.WaitForResult(func() (bool, error) {
		got = reader.raftTimings(sink)
		if len(got.appendEntries) == 0 {
			return false, fmt.Errorf("no AppendEntries time yet")
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })
	must.False(t, got.commitKnown)
	must.MapLen(t, 1, got.appendEntries)
	must.InDelta(t, 0.008, got.appendEntries["server-2"], 1e-9)
}

// TestMetricsStore_SampleServer checks that a server samples its own
// internals with both raft stores, and that they land in its store as series
// of their own.
func TestMetricsStore_SampleServer(t *testing.T) {
	ci.Parallel(t)

	for _, raftStore := range []string{RaftStoreBadger, RaftStoreBoltDB} {
		t.Run(raftStore, func(t *testing.T) {
			ci.Parallel(t)
			// The Raft library emits to the global telemetry, which the tests
			// do not set up: the test emits into the sink itself.
			const interval = 500 * time.Millisecond
			sink := metrics.NewInmemSink(interval, time.Minute)
			s, cleanup := TestServer(t, func(c *Config) {
				enableMetricsStore(t)(c)
				c.DevMode = false
				c.DataDir = t.TempDir()
				c.RaftStore = raftStore
				c.TelemetrySink = sink
			})
			defer cleanup()
			testutil.WaitForLeader(t, s.RPC)

			var got map[string]float64
			testutil.WaitForResult(func() (bool, error) {
				gauges, err := s.sampleGauges(time.Now())
				if err != nil {
					return false, err
				}
				got = gaugeValues(gauges)
				// JetStream may not run yet right after the start.
				if _, ok := got[metricsstore.MetricBusJetStreamBytes]; !ok {
					return false, fmt.Errorf("no %s", metricsstore.MetricBusJetStreamBytes)
				}
				return true, nil
			}, func(err error) { t.Fatal(err) })

			must.Positive(t, got[metricsstore.MetricServerRaftDBBytes])
			must.Positive(t, got[metricsstore.MetricServerRaftTerm])
			// The leader: no delay.
			must.Eq(t, 0.0, got[metricsstore.MetricServerRaftLastContact])
			must.MapContainsKey(t, got, metricsstore.MetricServerRaftCommitLag)
			must.MapContainsKey(t, got, metricsstore.MetricServerRaftFSMPending)
			must.MapContainsKey(t, got, metricsstore.MetricBusJetStreamMessages)
			must.Eq(t, 0.0, got[metricsstore.MetricBusRoutes])

			gauges, err := s.sampleGauges(time.Now())
			must.NoError(t, err)
			s.metricsStore.RecordGauges(time.Now(), gauges)
			series := map[string]bool{}
			for _, g := range s.metricsStore.Gauges() {
				series[g.Metric] = true
			}
			for _, metric := range []string{
				metricsstore.MetricServerRaftDBBytes,
				metricsstore.MetricServerRaftLastContact,
				metricsstore.MetricServerRaftCommitLag,
				metricsstore.MetricServerRaftFSMPending,
				metricsstore.MetricServerRaftTerm,
				metricsstore.MetricServerRPCRequests,
				metricsstore.MetricServerRPCErrors,
				metricsstore.MetricBusJetStreamBytes,
				metricsstore.MetricBusJetStreamMessages,
				metricsstore.MetricBusRoutes,
			} {
				must.True(t, series[metric], must.Sprint(metric))
			}
			// The waits for the leader were RPC requests.
			must.Positive(t, s.metricsCounters.rpcRequests.Load())

			// The Raft times reach the store through the sample loop.
			alignTelemetryInterval(interval, 200*time.Millisecond)
			sink.AddSample([]string{"nomad", "raft", "commitTime"}, 3)
			sink.AddSampleWithLabels([]string{"nomad", "raft", "replication", "appendEntries", "rpc"}, 2,
				[]metrics.Label{{Name: "peer_id", Value: "server-2"}})
			testutil.WaitForResult(func() (bool, error) {
				var commit, appendEntries bool
				for _, g := range s.metricsStore.Gauges() {
					switch g.Metric {
					case metricsstore.MetricServerRaftCommit:
						commit = true
					case metricsstore.MetricServerRaftAppendEntries:
						appendEntries = g.Labels["peer"] == "server-2"
					}
				}
				if !commit || !appendEntries {
					return false, fmt.Errorf("no Raft times in the store")
				}
				return true, nil
			}, func(err error) { t.Fatal(err) })
		})
	}
}

// TestMetricsStore_RPCCounters checks that a server counts the RPC requests
// it handles, in process and from the network, and those that fail: an
// error of the endpoint, an unknown method, and a request that fails on the
// connection.
func TestMetricsStore_RPCCounters(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	// The server makes requests of its own in the background: the counts
	// grow at least by those of the test.
	counts := func() (uint64, uint64) {
		return s.metricsCounters.rpcRequests.Load(), s.metricsCounters.rpcErrors.Load()
	}
	expect := func(name string, requests, errors uint64, call func()) {
		t.Helper()
		r0, e0 := counts()
		call()
		r1, e1 := counts()
		must.GreaterEq(t, r0+requests, r1, must.Sprint(name, ": requests"))
		must.GreaterEq(t, e0+errors, e1, must.Sprint(name, ": errors"))
	}
	ping := &structs.GenericRequest{QueryOptions: structs.QueryOptions{Region: s.config.Region}}
	register := &structs.JobRegisterRequest{WriteRequest: structs.WriteRequest{Region: s.config.Region}}

	expect("in process", 1, 0, func() {
		must.NoError(t, s.RPC("Status.Ping", ping, &struct{}{}))
	})
	expect("in process, endpoint error", 1, 1, func() {
		must.Error(t, s.RPC("Job.Register", register, &structs.JobRegisterResponse{}))
	})
	expect("in process, unknown method", 1, 1, func() {
		must.Error(t, s.RPC("Status.Nothing", ping, &struct{}{}))
	})

	// From the network the server counts a request after it answered it:
	// wait for the counts.
	network := func(name string, requests, errors uint64, call func()) {
		t.Helper()
		r0, e0 := counts()
		call()
		testutil.WaitForResult(func() (bool, error) {
			if r1, e1 := counts(); r1 < r0+requests || e1 < e0+errors {
				return false, fmt.Errorf("%s: requests %d, errors %d", name, r1-r0, e1-e0)
			}
			return true, nil
		}, func(err error) { t.Fatal(err) })
	}
	codec := rpcClient(t, s)
	defer codec.Close()
	network("network", 1, 0, func() {
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Status.Ping", ping, &struct{}{}))
	})
	network("network, endpoint error", 1, 1, func() {
		must.Error(t, msgpackrpc.CallWithCodec(codec, "Job.Register", register, &structs.JobRegisterResponse{}))
	})
	// An unknown method is answered, and the server closes the connection.
	network("network, unknown method", 1, 1, func() {
		must.Error(t, msgpackrpc.CallWithCodec(codec, "Status.Nothing", ping, &struct{}{}))
	})
}

// TestMetricsStore_ErrorNotingCodec checks that the codec of the RPC
// connections notes an answer with an error, and only that.
func TestMetricsStore_ErrorNotingCodec(t *testing.T) {
	ci.Parallel(t)

	c := &errorNotingCodec{ServerCodec: discardServerCodec{}}
	must.NoError(t, c.WriteResponse(&rpc.Response{ServiceMethod: "Status.Ping"}, struct{}{}))
	must.False(t, c.answeredError)
	must.NoError(t, c.WriteResponse(&rpc.Response{ServiceMethod: "Job.Register", Error: "missing job"}, nil))
	must.True(t, c.answeredError)
}

// discardServerCodec is a server codec that reads nothing and writes
// nowhere.
type discardServerCodec struct{}

func (discardServerCodec) ReadRequestHeader(*rpc.Request) error   { return io.EOF }
func (discardServerCodec) ReadRequestBody(any) error              { return io.EOF }
func (discardServerCodec) WriteResponse(*rpc.Response, any) error { return nil }
func (discardServerCodec) Close() error                           { return nil }
