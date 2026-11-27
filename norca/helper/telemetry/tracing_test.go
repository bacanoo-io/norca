// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"bytes"
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestTracing returns tracing bound to a provider with the agent's
// sampler at the given ratio, which records every ended span.
func newTestTracing(t testing.TB, ratio float64, s TracesSettings) (*Tracing, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(NewSampler(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return NewTracing(tp, s), exp
}

func TestTracing_Nil(t *testing.T) {
	ci.Parallel(t)

	var tr *Tracing
	must.Nil(t, tr.Tracer())
	must.Nil(t, tr.StartRPCServer("Job.Register", &RPCTestArgs{}))
	must.Nil(t, tr.StartChild(testTraceParent, "x", trace.SpanKindInternal))

	// not bound yet: no spans either
	tr = newTracing(TracesSettings{})
	must.Nil(t, tr.Tracer())
	must.Nil(t, tr.StartRPCServer("Job.Register", &RPCTestArgs{}))
}

func TestTracing_StartChild(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 0, TracesSettings{})

	// no parent, no span: forwarding and the Raft apply join traces only
	must.Nil(t, tr.StartChild("", "raft.apply", trace.SpanKindInternal))
	must.Nil(t, tr.StartChild("garbage", "raft.apply", trace.SpanKindInternal))

	span := tr.StartChild(testTraceParent, "raft.apply", trace.SpanKindInternal)
	must.NotNil(t, span)
	must.True(t, span.SpanContext().IsSampled(), must.Sprint("a sampled remote parent decides"))
	span.End()
	spans := exp.GetSpans()
	must.Len(t, 1, spans)
	must.Eq(t, testTraceID, spans[0].Parent.TraceID())
	must.Eq(t, testSpanID, spans[0].Parent.SpanID())

	unsampled := FormatTraceParent(testSpanContext(false))
	span = tr.StartChild(unsampled, "raft.apply", trace.SpanKindInternal)
	must.False(t, span.SpanContext().IsSampled())
	span.End()
	must.Len(t, 1, exp.GetSpans())
}

func TestTracing_ExcludedRPC(t *testing.T) {
	ci.Parallel(t)

	tr := newTracing(TracesSettings{ExcludeRPC: []string{"Variables.Read"}})
	for _, m := range []string{"Status.Ping", "Node.UpdateStatus", "Node.GetClientAllocs", "Node.UpdateAlloc", "Eval.Dequeue", "Variables.Read"} {
		must.True(t, tr.ExcludedRPC(m), must.Sprint(m))
	}
	must.False(t, tr.ExcludedRPC("Job.Register"))
}

func TestLogWith(t *testing.T) {
	ci.Parallel(t)

	var buf bytes.Buffer
	logger := hclog.New(&hclog.LoggerOptions{Output: &buf, Level: hclog.Debug, JSONFormat: true})

	must.True(t, logger == LogWith(logger, context.Background()))
	must.True(t, logger == LogWithTraceParent(logger, ""))

	ctx := trace.ContextWithSpanContext(context.Background(), testSpanContext(false))
	LogWith(logger, ctx).Debug("request complete")
	must.StrContains(t, buf.String(), `"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`)
	must.StrContains(t, buf.String(), `"span_id":"00f067aa0ba902b7"`)

	buf.Reset()
	LogWithTraceParent(logger, testTraceParent).Debug("x")
	must.StrContains(t, buf.String(), `"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`)
}
