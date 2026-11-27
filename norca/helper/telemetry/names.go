// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

// MetricsServiceName is the service name of the agent's go-metrics
// configuration: go-metrics puts it in front of every key, so every metric
// of the agent is named norca.<family>.<rest>, in the in-memory sink
// (/v1/metrics), the Prometheus sink (norca_<family>_<rest>) and the OTLP
// export alike.
//
// The families, by the first element of the go-metrics key:
//
//	server      the server (package norca and lib/auth/jwt): norca.server.rpc.request, norca.server.fsm.*, ...
//	client      the client agent (package client, the Consul service client): norca.client.allocs.*, norca.client.host.*, ...
//	raft        Raft (hashicorp/raft and the server's index gauges): norca.raft.apply, norca.raft.commitTime, ...
//	state       the state store index gauge of the server: norca.state.snapshotIndex
//	scheduler   the schedulers: norca.scheduler.allocs.reschedule.*
//	autoscaler  the built-in autoscaler on the leader: norca.autoscaler.*
//	badger      the Badger databases of the Raft log and the client state: norca.badger.<db>.*
//	agent       the agent itself: norca.agent.http.exceeded, norca.agent.otlp.export_errors, norca.agent.otlp.dropped
//	http        the HTTP API rate limit of the agent: norca.http.rate_limited
//	runtime     the Go runtime, emitted by go-metrics: norca.runtime.*
//	memberlist  the gossip of the servers: norca.memberlist.*
//	serf        the Serf cluster of the servers: norca.serf.*
//
// The former nomad.* names map one to one: nomad.nomad.* is norca.server.*,
// nomad.nomad.autoscaler.* is norca.autoscaler.*, nomad.nomad.badger.* is
// norca.badger.*, nomad.nomad.agent.http.* is norca.agent.http.*,
// nomad.nomad.http.* is norca.http.*, and every other nomad.<family>.* is
// norca.<family>.*; the rest of each key is unchanged.
const MetricsServiceName = "norca"
