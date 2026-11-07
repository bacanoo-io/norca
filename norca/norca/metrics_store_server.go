// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strconv"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
)

// The internals of a server (family 8) in the metrics store: its Raft log
// store and replication, and its system bus. Every server samples its own
// values into its own store; see the metric names in metricsstore.

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
}

// serverValues reads the internals of this server.
func (s *Server) serverValues() serverValues {
	var v serverValues
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

// serverGauges returns the gauges of the internals of a server. A value that
// is not known has no point.
func serverGauges(v serverValues) []metricsstore.Gauge {
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

	if b := v.bus; b != nil {
		if b.JetStream {
			add(metricsstore.MetricBusJetStreamBytes, float64(b.Bytes))
			add(metricsstore.MetricBusJetStreamMessages, float64(b.Messages))
		}
		add(metricsstore.MetricBusRoutes, float64(b.RouteServers))
	}
	return gauges
}
