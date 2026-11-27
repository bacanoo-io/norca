// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/raft"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The trace spans of the server around the RPC server spans (which the
// codec wrapper of telemetry.Tracing creates): forwarding, the Raft apply
// and the FSM apply on the leader. They only join traces: each one is a
// child of the traceparent its RPC arguments or Raft log entry carry, and
// without one there is no span. Every helper is a no-op without an otlp
// block (s.config.Tracing nil).

// Values of the norca.forward attribute.
const (
	forwardLeader = "leader"
	forwardServer = "server"
	forwardRegion = "region"
	forwardClient = "client"
)

// Span attributes of the Raft and FSM spans.
const (
	attrRaftMessageType = attribute.Key("norca.raft.message_type")
	attrRaftIndex       = attribute.Key("norca.raft.index")
	attrRaftSize        = attribute.Key("norca.raft.size")
)

// forwardSpan is the client span of a forwarded RPC. While the call is
// made, the arguments carry the forward span's traceparent, so the next
// server's RPC span is its child; end restores the server span's own.
type forwardSpan struct {
	span    trace.Span
	carrier telemetry.TraceCarrier
	prev    string
}

// startForward starts the span of forwarding method with args to a leader,
// a server, a region or a client. The zero forwardSpan (no tracing, no
// traceparent in args) does nothing.
func (s *Server) startForward(how, method string, args any) forwardSpan {
	tracing := s.config.Tracing
	if tracing == nil {
		return forwardSpan{}
	}
	carrier, ok := args.(telemetry.TraceCarrier)
	if !ok {
		return forwardSpan{}
	}
	prev := carrier.GetTraceParent()
	span := tracing.StartChild(prev, "forward "+method, trace.SpanKindClient,
		telemetry.AttrForward.String(how))
	if span == nil {
		return forwardSpan{}
	}
	carrier.SetTraceParent(telemetry.FormatTraceParent(span.SpanContext()))
	return forwardSpan{span: span, carrier: carrier, prev: prev}
}

// end ends the forward span with the result of the call.
func (f forwardSpan) end(err error) {
	if f.span == nil {
		return
	}
	f.carrier.SetTraceParent(f.prev)
	if err != nil {
		f.span.SetStatus(codes.Error, err.Error())
	}
	f.span.End()
}

// raftApplySpan is the span of a synchronous Raft apply (raftApply), from
// the submission of the entry to the FSM's response.
type raftApplySpan struct {
	span trace.Span
}

// startRaftApply starts the span of applying msg, a child of the
// traceparent msg carries. Messages without one (see the list of the
// tracing design) get no span.
func (s *Server) startRaftApply(t structs.MessageType, msg any) raftApplySpan {
	tracing := s.config.Tracing
	if tracing == nil {
		return raftApplySpan{}
	}
	carrier, ok := msg.(telemetry.TraceCarrier)
	if !ok {
		return raftApplySpan{}
	}
	t &^= structs.IgnoreUnknownTypeFlag
	span := tracing.StartChild(carrier.GetTraceParent(), "raft.apply "+t.String(), trace.SpanKindInternal,
		attrRaftMessageType.String(t.String()))
	return raftApplySpan{span: span}
}

// end ends the span with the index of the entry and the error of the apply.
func (r raftApplySpan) end(index uint64, err error) {
	if r.span == nil {
		return
	}
	if index > 0 {
		r.span.SetAttributes(attrRaftIndex.Int64(int64(index)))
	}
	if err != nil {
		r.span.SetStatus(codes.Error, err.Error())
	}
	r.span.End()
}

// raftLogExtensions returns the Extensions of the Raft log entry of msg:
// the traceparent the leader's FSM span is a child of (the Raft apply span
// when there is one, else the traceparent of msg). Only a sampled parent is
// carried, as the FSM span of an unsampled one would not be recorded; the
// entry then has no extensions, as without tracing.
func (s *Server) raftLogExtensions(msg any, span *raftApplySpan, size int) []byte {
	if s.config.Tracing == nil {
		return nil
	}
	var sc trace.SpanContext
	switch {
	case span != nil && span.span != nil:
		span.span.SetAttributes(attrRaftSize.Int(size))
		sc = span.span.SpanContext()
	default:
		carrier, ok := msg.(telemetry.TraceCarrier)
		if !ok {
			return nil
		}
		sc, _ = telemetry.ParseTraceParent(carrier.GetTraceParent())
	}
	if !sc.IsSampled() {
		return nil
	}
	return []byte(telemetry.FormatTraceParent(sc))
}

// fsmApplySpan starts the span of the FSM apply of a log entry on the
// leader, a child of the traceparent in the entry's extensions. Followers
// apply the same entries without a span: one span per server and write
// would be noise. It returns nil without tracing or a traceparent.
func (n *nomadFSM) fsmApplySpan(log *raft.Log) trace.Span {
	tracing := n.config.Tracing
	if tracing == nil || len(log.Extensions) == 0 || len(log.Data) == 0 {
		return nil
	}
	if n.config.IsLeader == nil || !n.config.IsLeader() {
		return nil
	}
	t := structs.MessageType(log.Data[0]) &^ structs.IgnoreUnknownTypeFlag
	return tracing.StartChild(string(log.Extensions), "fsm.apply "+t.String(), trace.SpanKindInternal,
		attrRaftMessageType.String(t.String()), attrRaftIndex.Int64(int64(log.Index)))
}

// endFSMApplySpan ends the FSM span with the error the FSM returned.
func endFSMApplySpan(span trace.Span, resp any) {
	if span == nil {
		return
	}
	if err, ok := resp.(error); ok && err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// evalTraceParent returns the traceparent that the evaluations enqueued by
// the entry being applied continue (the eval broker hands it to the worker
// that dequeues them): the entry's fsm.apply span when it has one (the
// leader, a sampled entry), otherwise the traceparent of req, the decoded
// request, which an unsampled request carries too. "" without tracing.
// Only the leader's broker is enabled, so a follower keeps nothing.
func (n *nomadFSM) evalTraceParent(req any) string {
	if n.config.Tracing == nil {
		return ""
	}
	if n.applySpan != nil {
		return telemetry.FormatTraceParent(n.applySpan.SpanContext())
	}
	if carrier, ok := req.(telemetry.TraceCarrier); ok {
		return carrier.GetTraceParent()
	}
	return ""
}

// startPlanSpan starts a span of the plan applier for a pending plan, a
// child of the Plan.Submit span that enqueued it. Without tracing or a
// traceparent it returns a span that records nothing.
func (s *Server) startPlanSpan(pending *pendingPlan, name string) trace.Span {
	span := s.config.Tracing.StartChild(pending.traceParent, name, trace.SpanKindInternal,
		telemetry.AttrEvalID.String(pending.plan.EvalID))
	if span == nil {
		return trace.SpanFromContext(context.Background())
	}
	return span
}

// Span attributes of the plan applier's spans.
const (
	attrPlanRefreshIndex  = attribute.Key("norca.plan.refresh_index")
	attrPlanRejectedNodes = attribute.Key("norca.plan.rejected_nodes")
	attrPlanAllocIndex    = attribute.Key("norca.plan.alloc_index")
)

// endPlanEvaluateSpan ends the plan.evaluate span with the outcome of the
// evaluation: a forced state refresh and the nodes rejected.
func endPlanEvaluateSpan(span trace.Span, result *structs.PlanResult, err error) {
	if result != nil && span.IsRecording() {
		if result.RefreshIndex != 0 {
			span.SetAttributes(attrPlanRefreshIndex.Int64(int64(result.RefreshIndex)))
		}
		if n := len(result.RejectedNodes); n > 0 {
			span.SetAttributes(attrPlanRejectedNodes.Int(n))
		}
	}
	telemetry.EndSpan(span, err)
}

// endPlanApplySpan ends the plan.apply span once the plan's Raft entry is
// committed, with its index or the error of the apply.
func endPlanApplySpan(span trace.Span, future raft.ApplyFuture, err error) {
	if err == nil && span.IsRecording() {
		span.SetAttributes(attrPlanAllocIndex.Int64(int64(future.Index())))
	}
	telemetry.EndSpan(span, err)
}
