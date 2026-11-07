// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/testutil"
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
	}
	must.Eq(t, map[string]float64{
		metricsstore.MetricServerRaftDBBytes:     1 << 20,
		metricsstore.MetricServerRaftLastContact: 1.5,
		metricsstore.MetricServerRaftCommitLag:   3,
		metricsstore.MetricServerRaftFSMPending:  2,
		metricsstore.MetricServerRaftTerm:        4,
		metricsstore.MetricBusJetStreamBytes:     4096,
		metricsstore.MetricBusJetStreamMessages:  12,
		metricsstore.MetricBusRoutes:             2,
	}, gaugeValues(serverGauges(values)))

	// The leader has no delay; a server that never heard from a leader has
	// no point. An applied index ahead of the commit index (after a restart)
	// is no lag.
	values.raft["last_contact"] = "0"
	values.raft["applied_index"] = "130"
	got := gaugeValues(serverGauges(values))
	must.Eq(t, 0.0, got[metricsstore.MetricServerRaftLastContact])
	must.MapContainsKey(t, got, metricsstore.MetricServerRaftLastContact)
	must.Eq(t, 0.0, got[metricsstore.MetricServerRaftCommitLag])
	values.raft["last_contact"] = "never"
	got = gaugeValues(serverGauges(values))
	must.MapNotContainsKey(t, got, metricsstore.MetricServerRaftLastContact)

	// No size of the raft store, no JetStream, no Raft: no points for them.
	values = serverValues{bus: &bus.Sample{RouteServers: 1}}
	must.Eq(t, map[string]float64{metricsstore.MetricBusRoutes: 1}, gaugeValues(serverGauges(values)))
	must.SliceEmpty(t, serverGauges(serverValues{}))
}

// TestMetricsStore_SampleServer checks that a server samples its own
// internals with both raft stores, and that they land in its store as series
// of their own.
func TestMetricsStore_SampleServer(t *testing.T) {
	ci.Parallel(t)

	for _, raftStore := range []string{RaftStoreBadger, RaftStoreBoltDB} {
		t.Run(raftStore, func(t *testing.T) {
			ci.Parallel(t)
			s, cleanup := TestServer(t, func(c *Config) {
				enableMetricsStore(t)(c)
				c.DevMode = false
				c.DataDir = t.TempDir()
				c.RaftStore = raftStore
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
				metricsstore.MetricBusJetStreamBytes,
				metricsstore.MetricBusJetStreamMessages,
				metricsstore.MetricBusRoutes,
			} {
				must.True(t, series[metric], must.Sprint(metric))
			}
		})
	}
}
