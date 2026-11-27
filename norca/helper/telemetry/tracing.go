// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bacanoo-io/norca/norca/version"
	"github.com/hashicorp/go-hclog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// InstrumentationName is the instrumentation scope of every span the agent
// creates.
const InstrumentationName = "github.com/bacanoo-io/norca/norca"

// Log keys of the trace correlation, as the log key convention names them.
const (
	LogKeyTraceID = "trace_id"
	LogKeySpanID  = "span_id"
)

// TraceIDHeader is the response header of the HTTP API that carries the
// trace ID of the request, so a user can quote it.
const TraceIDHeader = "Norca-Trace-Id"

// Span attributes of the RPC spans.
const (
	AttrRPCSystem  = attribute.Key("rpc.system")
	AttrRPCService = attribute.Key("rpc.service")
	AttrRPCMethod  = attribute.Key("rpc.method")

	// AttrForward is how a forwarding span forwards: leader, server,
	// region or client.
	AttrForward = attribute.Key("norca.forward")
)

// rpcSystem is the rpc.system of Norca's msgpack RPC.
var rpcSystem = AttrRPCSystem.String("norca")

// TraceCarrier is an RPC argument that carries the W3C traceparent of its
// caller: structs.InternalRpcInfo, which the query and write options of
// almost every RPC argument embed.
type TraceCarrier interface {
	GetTraceParent() string
	SetTraceParent(string)
}

// blockingQuery is an RPC argument that may block: structs.QueryOptions.
type blockingQuery interface {
	TimeToBlock() time.Duration
}

// Tracing is the trace instrumentation of an agent: its tracer and the
// rules of what is traced (exclusions, trust in incoming HTTP trace
// context). It exists whenever the agent has an otlp block; the server,
// the client and the HTTP API install their wrappers only then, so an
// agent without the block does no tracing work at all.
//
// A nil *Tracing is valid and traces nothing. Before the providers start,
// a non-nil one traces nothing either: its tracer is bound at start.
type Tracing struct {
	tracer          atomic.Pointer[tracerRef]
	trustHTTPParent bool
	excludedRPC     map[string]struct{}

	// rpcOptions caches the span start options per RPC method.
	rpcOptions sync.Map
}

type tracerRef struct{ trace.Tracer }

// newTracing creates the tracing of the traces settings, not yet bound to
// a tracer provider.
func newTracing(s TracesSettings) *Tracing {
	t := &Tracing{
		trustHTTPParent: s.TrustHTTPParent,
		excludedRPC:     setOf(slices.Concat(excludedRPC, s.ExcludeRPC)...),
	}
	return t
}

// NewTracing returns tracing bound to a tracer provider at once, for tests
// and for programs that build their own provider (with NewSampler).
func NewTracing(tp trace.TracerProvider, s TracesSettings) *Tracing {
	t := newTracing(s)
	t.bind(tp)
	return t
}

// bind makes the tracing use the tracer of tp from now on.
func (t *Tracing) bind(tp trace.TracerProvider) {
	t.tracer.Store(&tracerRef{tp.Tracer(InstrumentationName,
		trace.WithInstrumentationVersion(version.GetVersion().VersionNumber()))})
}

// Tracer returns the agent's tracer, or nil when t is nil or not bound yet.
func (t *Tracing) Tracer() trace.Tracer {
	if t == nil {
		return nil
	}
	if ref := t.tracer.Load(); ref != nil {
		return ref.Tracer
	}
	return nil
}

// ExcludedRPC reports whether an RPC method never gets a span.
func (t *Tracing) ExcludedRPC(method string) bool {
	_, ok := t.excludedRPC[method]
	return ok
}

// StartRPCServer starts the server span of an RPC whose arguments were
// decoded into args. The parent is the traceparent in args; the span's own
// traceparent is written back into args, so the code serving the request
// (forwarding, Raft apply) continues the trace from the arguments alone.
// It returns nil, and starts nothing, when t traces nothing, the method is
// excluded or the request is a blocking query.
func (t *Tracing) StartRPCServer(method string, args any) trace.Span {
	tracer := t.Tracer()
	if tracer == nil || t.ExcludedRPC(method) {
		return nil
	}
	if b, ok := args.(blockingQuery); ok && b.TimeToBlock() > 0 {
		return nil
	}
	ctx := context.Background()
	carrier, isCarrier := args.(TraceCarrier)
	if isCarrier {
		ctx = ContextWithTraceParent(ctx, carrier.GetTraceParent())
	}
	_, span := tracer.Start(ctx, method, t.rpcServerOptions(method)...)
	if isCarrier {
		carrier.SetTraceParent(FormatTraceParent(span.SpanContext()))
	}
	return span
}

// rpcServerOptions returns the start options of the server spans of an RPC
// method, made once per method: the attributes would otherwise be the
// largest allocation of an RPC span. The SDK copies them into the span, so
// the cached slice is shared safely.
func (t *Tracing) rpcServerOptions(method string) []trace.SpanStartOption {
	if opts, ok := t.rpcOptions.Load(method); ok {
		return opts.([]trace.SpanStartOption)
	}
	service, name, _ := strings.Cut(method, ".")
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(rpcSystem, AttrRPCService.String(service), AttrRPCMethod.String(name)),
	}
	// net/rpc serves registered methods only, so the cache is bounded by
	// the endpoints.
	actual, _ := t.rpcOptions.LoadOrStore(method, opts)
	return actual.([]trace.SpanStartOption)
}

// StartChild starts a span as a child of a W3C traceparent, as RPC
// arguments and Raft log entries carry it. Without a valid parent it starts
// nothing and returns nil: the code that only has a traceparent (forwarding,
// Raft apply, the FSM) joins traces, it does not start them.
func (t *Tracing) StartChild(traceParent, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) trace.Span {
	tracer := t.Tracer()
	if tracer == nil {
		return nil
	}
	sc, ok := ParseTraceParent(traceParent)
	if !ok {
		return nil
	}
	_, span := tracer.Start(trace.ContextWithRemoteSpanContext(context.Background(), sc), name,
		trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	return span
}

// LogWith returns logger with the trace_id and span_id of the span in ctx,
// or logger itself when ctx carries no valid span context (no otlp block,
// or an excluded request). An unsampled span has IDs too, so the log lines
// of one request correlate even when its trace was not exported.
func LogWith(logger hclog.Logger, ctx context.Context) hclog.Logger {
	return LogWithSpanContext(logger, trace.SpanContextFromContext(ctx))
}

// LogWithSpanContext is LogWith for a span context at hand.
func LogWithSpanContext(logger hclog.Logger, sc trace.SpanContext) hclog.Logger {
	if !sc.IsValid() {
		return logger
	}
	return logger.With(LogKeyTraceID, sc.TraceID().String(), LogKeySpanID, sc.SpanID().String())
}

// LogWithTraceParent is LogWith for a traceparent from RPC arguments.
func LogWithTraceParent(logger hclog.Logger, traceParent string) hclog.Logger {
	sc, ok := ParseTraceParent(traceParent)
	if !ok {
		return logger
	}
	return LogWithSpanContext(logger, sc)
}

// lazyTracerProvider hands out a tracer that starts its spans with the
// tracer t is bound to when the span starts, for instrumentation that takes
// its tracer when it is built (otelhttp), before the providers start.
type lazyTracerProvider struct {
	embedded.TracerProvider
	t *Tracing
}

func (p lazyTracerProvider) Tracer(string, ...trace.TracerOption) trace.Tracer {
	return lazyTracer{t: p.t}
}

type lazyTracer struct {
	embedded.Tracer
	t *Tracing
}

func (l lazyTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	tracer := l.t.Tracer()
	if tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return tracer.Start(ctx, name, opts...)
}
