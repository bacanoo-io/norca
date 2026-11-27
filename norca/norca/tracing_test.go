// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestTracing returns tracing that samples every new root and records
// every ended span.
func newTestTracing(t *testing.T) (*telemetry.Tracing, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(telemetry.NewSampler(sdktrace.AlwaysSample())),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return telemetry.NewTracing(tp, telemetry.TracesSettings{}), exp
}

// traceSpans returns the spans of one trace by their span IDs.
func traceSpans(exp *tracetest.InMemoryExporter, id trace.TraceID) map[trace.SpanID]tracetest.SpanStub {
	out := map[trace.SpanID]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		if s.SpanContext.TraceID() == id {
			out[s.SpanContext.SpanID()] = s
		}
	}
	return out
}

// childNamed returns the one span of spans named name whose parent is
// parent.
func childNamed(t *testing.T, spans map[trace.SpanID]tracetest.SpanStub, parent trace.SpanID, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name && s.Parent.SpanID() == parent {
			found = append(found, s)
		}
	}
	must.Len(t, 1, found, must.Sprintf("children %q of %s", name, parent))
	return found[0]
}

func mustParseUint(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	must.NoError(t, err)
	return v
}

func attr(s tracetest.SpanStub, key attribute.Key) string {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// A job registered through a follower yields one trace: the caller's span
// (the HTTP API in the agent) → Job.Register on the follower → its forward
// to the leader → Job.Register on the leader → the Raft apply → the FSM
// apply on the leader.
func TestTracing_ForwardedWrite(t *testing.T) {
	ci.Parallel(t)

	tracing, exp := newTestTracing(t)
	servers := make([]*Server, 3)
	for i := range servers {
		s, cleanup := TestServer(t, func(c *Config) {
			c.BootstrapExpect = 3
			c.Tracing = tracing
		})
		t.Cleanup(cleanup)
		servers[i] = s
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	var follower *Server
	for _, s := range servers {
		if !s.IsLeader() {
			follower = s
			break
		}
	}
	must.NotNil(t, follower)

	// the caller's span, as the HTTP API starts it
	_, root := tracing.Tracer().Start(context.Background(), "PUT /v1/jobs", trace.WithSpanKind(trace.SpanKindServer))
	req := &structs.JobRegisterRequest{
		Job:          mock.Job(),
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace},
	}
	req.SetTraceParent(telemetry.FormatTraceParent(root.SpanContext()))
	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, follower), "Job.Register", req, &resp))
	root.End()

	rootSC := root.SpanContext()
	spans := traceSpans(exp, rootSC.TraceID())

	onFollower := childNamed(t, spans, rootSC.SpanID(), "Job.Register")
	must.Eq(t, trace.SpanKindServer, onFollower.SpanKind)
	must.Eq(t, "norca", attr(onFollower, "rpc.system"))
	must.Eq(t, "Job", attr(onFollower, "rpc.service"))
	must.Eq(t, "Register", attr(onFollower, "rpc.method"))

	forward := childNamed(t, spans, onFollower.SpanContext.SpanID(), "forward Job.Register")
	must.Eq(t, trace.SpanKindClient, forward.SpanKind)
	must.Eq(t, "leader", attr(forward, telemetry.AttrForward))

	onLeader := childNamed(t, spans, forward.SpanContext.SpanID(), "Job.Register")
	apply := childNamed(t, spans, onLeader.SpanContext.SpanID(), "raft.apply JobRegisterRequestType")
	must.Eq(t, "JobRegisterRequestType", attr(apply, "norca.raft.message_type"))
	must.Eq(t, resp.JobModifyIndex, mustParseUint(t, attr(apply, "norca.raft.index")))
	must.NotEq(t, "", attr(apply, "norca.raft.size"))

	fsm := childNamed(t, spans, apply.SpanContext.SpanID(), "fsm.apply JobRegisterRequestType")
	must.Eq(t, attr(apply, "norca.raft.index"), attr(fsm, "norca.raft.index"))

	// one FSM span: the followers apply the entry without one
	var fsmSpans int
	for _, s := range spans {
		if s.Name == "fsm.apply JobRegisterRequestType" {
			fsmSpans++
		}
	}
	must.Eq(t, 1, fsmSpans)
}

// Excluded methods and blocking queries get no span, even with a parent.
func TestTracing_Excluded(t *testing.T) {
	ci.Parallel(t)

	tracing, exp := newTestTracing(t)
	s, cleanup := TestServer(t, func(c *Config) { c.Tracing = tracing })
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	_, root := tracing.Tracer().Start(context.Background(), "GET /v1/jobs")
	tp := telemetry.FormatTraceParent(root.SpanContext())
	codec := rpcClient(t, s)

	ping := &structs.GenericRequest{}
	ping.SetTraceParent(tp)
	var out struct{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Status.Ping", ping, &out))

	list := &structs.JobListRequest{QueryOptions: structs.QueryOptions{
		Region: "global", Namespace: structs.DefaultNamespace, MinQueryIndex: 1, MaxQueryTime: 10 * time.Millisecond,
	}}
	list.SetTraceParent(tp)
	var jobs structs.JobListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.List", list, &jobs))
	root.End()

	spans := traceSpans(exp, root.SpanContext().TraceID())
	must.MapLen(t, 1, spans, must.Sprint("only the caller's span"))
}

// Raft entries carry a traceparent only when it is sampled.
func TestTracing_RaftLogExtensions(t *testing.T) {
	ci.Parallel(t)

	s := &Server{config: &Config{}}
	req := &structs.JobRegisterRequest{}
	must.Nil(t, s.raftLogExtensions(req, nil, 10), must.Sprint("no tracing"))

	tracing, _ := newTestTracing(t)
	s.config.Tracing = tracing
	must.Nil(t, s.raftLogExtensions(req, nil, 10), must.Sprint("no traceparent"))

	sampled := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req.SetTraceParent(sampled)
	must.Eq(t, []byte(sampled), s.raftLogExtensions(req, nil, 10))

	req.SetTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	must.Nil(t, s.raftLogExtensions(req, nil, 10))

	// an FSM without the leader's role or without the entry's traceparent
	// does not trace the apply
	fsm := &nomadFSM{config: &FSMConfig{Tracing: tracing, IsLeader: func() bool { return false }}}
	must.Nil(t, fsm.fsmApplySpan(&raft.Log{Data: []byte{byte(structs.JobRegisterRequestType)}, Extensions: []byte(sampled)}))
	fsm.config.IsLeader = func() bool { return true }
	must.Nil(t, fsm.fsmApplySpan(&raft.Log{Data: []byte{byte(structs.JobRegisterRequestType)}}))
	span := fsm.fsmApplySpan(&raft.Log{Data: []byte{byte(structs.JobRegisterRequestType)}, Extensions: []byte(sampled)})
	must.NotNil(t, span)
	span.End()
}
