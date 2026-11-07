// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	metrics "github.com/hashicorp/go-metrics"
)

// The internals of a server (family 8) in the metrics store: its Raft log
// store and replication, its RPC requests, and its system bus. Every server
// samples its own values into its own store; see the metric names in
// metricsstore.

// serverValues are the internals a server reads of itself for a sample.
type serverValues struct {
	// raftDBBytes is the size of the Raft log store on disk; raftDBKnown is
	// false when the store cannot tell (a server in dev mode keeps its log in
	// memory).
	raftDBBytes int64
	raftDBKnown bool

	// raft is the output of raft.Stats(); nil without Raft.
	raft map[string]string

	// bus is the sample of the system bus; nil without a bus or when it
	// could not be read.
	bus *bus.Sample

	// timings are the Raft times of the leader since the last sample.
	timings raftTimings

	// The RPC requests this server handled and those that failed, counted
	// since the server started.
	rpcRequests uint64
	rpcErrors   uint64
}

// serverValues reads the internals of this server.
func (s *Server) serverValues() serverValues {
	v := serverValues{
		timings:     s.metricsTelemetry.raftTimings(s.config.TelemetrySink),
		rpcRequests: s.metricsCounters.rpcRequests.Load(),
		rpcErrors:   s.metricsCounters.rpcErrors.Load(),
	}
	if sizer, ok := s.raftStore.(raftStoreSizer); ok {
		size, err := sizer.DiskSize()
		if err == nil {
			v.raftDBBytes, v.raftDBKnown = size, true
		} else {
			s.logger.Debug("metrics store: the size of the raft store is not known", "error", err)
		}
	}
	if s.raft != nil {
		v.raft = s.raft.Stats()
	}
	if s.bus != nil {
		sample, err := s.bus.Sample()
		if err == nil {
			v.bus = sample
		} else {
			s.logger.Debug("metrics store: sampling the system bus failed", "error", err)
		}
	}
	return v
}

// serverGauges returns the gauges of the internals of a server; counters
// turns the RPC counters into their increase since the last sample. A value
// that is not known has no point.
func serverGauges(v serverValues, counters *counterSampler) []metricsstore.Gauge {
	var gauges []metricsstore.Gauge
	add := func(metric string, value float64) {
		gauges = append(gauges, metricsstore.Gauge{Metric: metric, Value: value})
	}
	if v.raftDBKnown {
		add(metricsstore.MetricServerRaftDBBytes, float64(v.raftDBBytes))
	}

	if v.raft != nil {
		number := func(key string) (uint64, bool) {
			n, err := strconv.ParseUint(v.raft[key], 10, 64)
			return n, err == nil
		}
		// "0" on the leader, "never" on a server that never heard from a
		// leader, and the time since the last contact otherwise.
		switch contact := v.raft["last_contact"]; contact {
		case "0":
			add(metricsstore.MetricServerRaftLastContact, 0)
		case "", "never":
		default:
			if d, err := time.ParseDuration(contact); err == nil {
				add(metricsstore.MetricServerRaftLastContact, d.Seconds())
			}
		}
		commit, okCommit := number("commit_index")
		applied, okApplied := number("applied_index")
		if okCommit && okApplied {
			// After a restart a server applies its snapshot before it
			// learns the commit index of the leader: no lag then.
			lag := 0.0
			if commit > applied {
				lag = float64(commit - applied)
			}
			add(metricsstore.MetricServerRaftCommitLag, lag)
		}
		if n, ok := number("fsm_pending"); ok {
			add(metricsstore.MetricServerRaftFSMPending, float64(n))
		}
		if n, ok := number("term"); ok {
			add(metricsstore.MetricServerRaftTerm, float64(n))
		}
	}

	if t := v.timings; t.commitKnown {
		add(metricsstore.MetricServerRaftCommit, t.commit)
	}
	peers := make([]string, 0, len(v.timings.appendEntries))
	for peer := range v.timings.appendEntries {
		peers = append(peers, peer)
	}
	sort.Strings(peers)
	for _, peer := range peers {
		gauges = append(gauges, metricsstore.Gauge{Metric: metricsstore.MetricServerRaftAppendEntries,
			Labels: map[string]string{"peer": peer}, Value: v.timings.appendEntries[peer]})
	}

	add(metricsstore.MetricServerRPCRequests,
		counters.increase(metricsstore.MetricServerRPCRequests, v.rpcRequests))
	add(metricsstore.MetricServerRPCErrors,
		counters.increase(metricsstore.MetricServerRPCErrors, v.rpcErrors))

	if b := v.bus; b != nil {
		if b.JetStream {
			add(metricsstore.MetricBusJetStreamBytes, float64(b.Bytes))
			add(metricsstore.MetricBusJetStreamMessages, float64(b.Messages))
		}
		add(metricsstore.MetricBusRoutes, float64(b.RouteServers))
	}
	return gauges
}

// The names of the Raft times in the telemetry, after the service name
// ("nomad"): the time to commit an entry, and the time of an AppendEntries
// request to a follower (label peer_id). The raft library emits the second
// once more under a name that ends in the peer; that one is not read.
const (
	telemetryRaftCommit        = "raft.commitTime"
	telemetryRaftAppendEntries = "raft.replication.appendEntries.rpc"
	telemetryRaftPeerLabel     = "peer_id"
)

// raftTimings are the mean Raft times of the leader in seconds over the
// telemetry intervals read for a sample. A server that is not the leader has
// none.
type raftTimings struct {
	commit      float64
	commitKnown bool

	// appendEntries is the mean time of the AppendEntries requests by
	// follower (Raft server ID).
	appendEntries map[string]float64
}

// telemetryReader reads the Raft times from the in-memory sink of the
// agent's telemetry. The sink keeps 10-second intervals for a minute; every
// read takes the intervals that ended since the previous read, so each
// interval counts once. With a sample interval above 50 seconds only the
// last 50 seconds count.
type telemetryReader struct {
	mu sync.Mutex
	// last is the start of the newest interval read.
	last time.Time
}

// raftTimings returns the mean Raft times of the intervals of sink that ended
// since the last call; none without a sink.
func (r *telemetryReader) raftTimings(sink *metrics.InmemSink) raftTimings {
	var t raftTimings
	if sink == nil {
		return t
	}
	intervals := sink.Data()
	if len(intervals) == 0 {
		return t
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	type mean struct{ sum, count float64 }
	var commit mean
	peers := map[string]*mean{}
	// The last interval is the current one, not complete yet.
	for _, interval := range intervals[:len(intervals)-1] {
		if !interval.Interval.After(r.last) {
			continue
		}
		r.last = interval.Interval
		interval.RLock()
		for _, sample := range interval.Samples {
			if sample.AggregateSample == nil || sample.Count == 0 {
				continue
			}
			switch {
			case telemetryNameIs(sample.Name, telemetryRaftCommit):
				commit.sum += sample.Sum
				commit.count += float64(sample.Count)
			case telemetryNameIs(sample.Name, telemetryRaftAppendEntries):
				peer := telemetryLabel(sample.Labels, telemetryRaftPeerLabel)
				if peer == "" {
					continue
				}
				m := peers[peer]
				if m == nil {
					m = &mean{}
					peers[peer] = m
				}
				m.sum += sample.Sum
				m.count += float64(sample.Count)
			}
		}
		interval.RUnlock()
	}

	// The agent's telemetry measures times in milliseconds (the default
	// TimerGranularity of go-metrics).
	const toSeconds = float64(time.Millisecond) / float64(time.Second)
	if commit.count > 0 {
		t.commit, t.commitKnown = commit.sum/commit.count*toSeconds, true
	}
	if len(peers) > 0 {
		t.appendEntries = make(map[string]float64, len(peers))
		for peer, m := range peers {
			t.appendEntries[peer] = m.sum / m.count * toSeconds
		}
	}
	return t
}

// telemetryNameIs reports whether the telemetry name is key, with or without
// the service name and other parts in front of it.
func telemetryNameIs(name, key string) bool {
	return name == key || strings.HasSuffix(name, "."+key)
}

// telemetryLabel returns the value of the label name, "" without it.
func telemetryLabel(labels []metrics.Label, name string) string {
	for _, l := range labels {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}
