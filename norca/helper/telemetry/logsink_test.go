// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// memProcessor keeps the records emitted to its provider.
type memProcessor struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (m *memProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, r.Clone())
	return nil
}

func (*memProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (*memProcessor) Shutdown(context.Context) error                         { return nil }
func (*memProcessor) ForceFlush(context.Context) error                       { return nil }

func (m *memProcessor) all() []sdklog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sdklog.Record(nil), m.records...)
}

// boundSink returns a sink at level bound to an in-memory provider.
func boundSink(t *testing.T, level hclog.Level) (*LogSink, *memProcessor) {
	t.Helper()
	mem := &memProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(mem))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	sink := NewLogSink(level)
	sink.Bind(lp.Logger(InstrumentationName))
	return sink, mem
}

func recordAttrs(r sdklog.Record) map[string]otellog.Value {
	out := map[string]otellog.Value{}
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		out[kv.Key] = kv.Value
		return true
	})
	return out
}

type stringer struct{}

func (stringer) String() string { return "stringer" }

type nilStringer struct{ name *string }

func (n *nilStringer) String() string { return *n.name }

func TestLogSink_Severity(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	for _, level := range []hclog.Level{hclog.Trace, hclog.Debug, hclog.Info, hclog.Warn, hclog.Error} {
		sink.Accept("agent", level, "line")
	}
	records := mem.all()
	must.Len(t, 5, records)
	expected := []struct {
		number otellog.Severity
		text   string
	}{
		{otellog.SeverityTrace, "TRACE"},
		{otellog.SeverityDebug, "DEBUG"},
		{otellog.SeverityInfo, "INFO"},
		{otellog.SeverityWarn, "WARN"},
		{otellog.SeverityError, "ERROR"},
	}
	for i, r := range records {
		must.Eq(t, expected[i].number, r.Severity())
		must.Eq(t, expected[i].text, r.SeverityText())
	}
	// the numbers of the log data model
	for i, number := range []int{1, 5, 9, 13, 17} {
		must.Eq(t, otellog.Severity(number), records[i].Severity())
	}
}

func TestLogSink_Level(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Warn)
	must.Eq(t, hclog.Warn, sink.Level())
	sink.Accept("agent", hclog.Info, "below")
	sink.Accept("agent", hclog.Warn, "at")
	sink.Accept("agent", hclog.Off, "off")
	sink.Accept("agent", hclog.NoLevel, "no level")
	must.Len(t, 1, mem.all())

	// a reload of logs.level
	sink.SetLevel(hclog.Debug)
	sink.Accept("agent", hclog.Debug, "now exported")
	must.Len(t, 2, mem.all())

	// stopped at the shutdown
	sink.close()
	sink.Accept("agent", hclog.Error, "after the shutdown")
	must.Len(t, 2, mem.all())
}

func TestLogSink_Record(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	sink.now = func() time.Time { return now }
	sink.Accept("agent.client.alloc_runner", hclog.Info, "task started",
		"alloc_id", "a1", "count", 3, "duration", 1500*time.Microsecond)

	records := mem.all()
	must.Len(t, 1, records)
	r := records[0]
	must.Eq(t, now, r.Timestamp())
	must.Eq(t, "task started", r.Body().AsString())
	must.Eq(t, InstrumentationName, r.InstrumentationScope().Name)
	attrs := recordAttrs(r)
	must.Eq(t, "agent.client.alloc_runner", attrs[AttrLogger].AsString())
	must.Eq(t, "a1", attrs["alloc_id"].AsString())
	must.Eq(t, int64(3), attrs["count"].AsInt64())
	must.Eq(t, 1.5, attrs["duration"].AsFloat64(), must.Sprint("a duration is in milliseconds"))
	must.False(t, r.TraceID().IsValid())
}

func TestLogValue(t *testing.T) {
	ci.Parallel(t)

	when := time.Date(2026, 11, 27, 12, 0, 0, 5, time.UTC)
	tests := []struct {
		name     string
		value    any
		expected otellog.Value
	}{
		{name: "nil", value: nil, expected: otellog.Value{}},
		{name: "string", value: "s", expected: otellog.StringValue("s")},
		{name: "bool", value: true, expected: otellog.BoolValue(true)},
		{name: "int", value: -4, expected: otellog.Int64Value(-4)},
		{name: "int8", value: int8(-8), expected: otellog.Int64Value(-8)},
		{name: "int16", value: int16(16), expected: otellog.Int64Value(16)},
		{name: "int32", value: int32(32), expected: otellog.Int64Value(32)},
		{name: "int64", value: int64(64), expected: otellog.Int64Value(64)},
		{name: "uint8", value: uint8(8), expected: otellog.Int64Value(8)},
		{name: "uint16", value: uint16(16), expected: otellog.Int64Value(16)},
		{name: "uint32", value: uint32(32), expected: otellog.Int64Value(32)},
		{name: "uint", value: uint(7), expected: otellog.Int64Value(7)},
		{name: "uint64", value: uint64(64), expected: otellog.Int64Value(64)},
		{name: "uint64 too large", value: uint64(math.MaxUint64), expected: otellog.StringValue("18446744073709551615")},
		{name: "float32", value: float32(0.5), expected: otellog.Float64Value(0.5)},
		{name: "float64", value: 2.25, expected: otellog.Float64Value(2.25)},
		{name: "duration", value: 2 * time.Second, expected: otellog.Float64Value(2000)},
		{name: "time", value: when, expected: otellog.StringValue("2026-11-27T12:00:00.000000005Z")},
		{name: "bytes", value: []byte{1, 2}, expected: otellog.BytesValue([]byte{1, 2})},
		{name: "strings", value: []string{"a", "b"}, expected: otellog.SliceValue(otellog.StringValue("a"), otellog.StringValue("b"))},
		{name: "hex", value: hclog.Hex(255), expected: otellog.Int64Value(255)},
		{name: "octal", value: hclog.Octal(8), expected: otellog.Int64Value(8)},
		{name: "binary", value: hclog.Binary(2), expected: otellog.Int64Value(2)},
		{name: "quote", value: hclog.Quote("q"), expected: otellog.StringValue("q")},
		{name: "format", value: hclog.Fmt("%d-%s", 1, "x"), expected: otellog.StringValue("1-x")},
		{name: "empty format", value: hclog.Format{}, expected: otellog.StringValue("")},
		{name: "error", value: errors.New("boom"), expected: otellog.StringValue("boom")},
		{name: "stringer", value: stringer{}, expected: otellog.StringValue("stringer")},
		{name: "panicking stringer", value: &nilStringer{}, expected: otellog.StringValue(fmt.Sprint(&nilStringer{}))},
		{name: "other", value: struct{ A int }{A: 1}, expected: otellog.StringValue("{1}")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.True(t, tt.expected.Equal(logValue(tt.value)), must.Sprintf("got %v", logValue(tt.value)))
		})
	}
}

func TestLogSink_Error(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	sink.Accept("agent", hclog.Error, "failed", "error", fmt.Errorf("wrapped: %w", io.EOF))
	sink.Accept("agent", hclog.Error, "failed", "error", "only a string", "cause_error", io.EOF)

	records := mem.all()
	must.Len(t, 2, records)
	attrs := recordAttrs(records[0])
	must.Eq(t, "wrapped: EOF", attrs["exception.message"].AsString())
	// the SDK names the type of the wrapped error
	must.Eq(t, "*errors.errorString", attrs["exception.type"].AsString())
	_, kept := attrs["error"]
	must.False(t, kept, must.Sprint("the error is the record's error"))

	attrs = recordAttrs(records[1])
	must.Eq(t, "only a string", attrs["error"].AsString())
	must.Eq(t, "EOF", attrs["cause_error"].AsString())
	_, set := attrs["exception.message"]
	must.False(t, set)
}

type typedError struct{}

func (typedError) Error() string     { return "typed" }
func (typedError) ErrorType() string { return "norca.typed" }

// A line with an error and a captured stack trace keeps both: the SDK skips
// exception.message and exception.type on a record with an exception.*
// attribute, so the sink sets them the way the SDK would.
func TestLogSink_ErrorWithStacktrace(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	err := fmt.Errorf("wrapped: %w", io.EOF)
	sink.Accept("agent", hclog.Error, "failed", "error", err)
	sink.Accept("agent", hclog.Error, "failed", "error", err, hclog.CapturedStacktrace("goroutine 1 [running]"))
	sink.Accept("agent", hclog.Error, "failed", "error", typedError{}, hclog.CapturedStacktrace("stack"))

	records := mem.all()
	must.Len(t, 3, records)
	bySDK, withStack := recordAttrs(records[0]), recordAttrs(records[1])
	must.Eq(t, "goroutine 1 [running]", withStack["exception.stacktrace"].AsString())
	must.Eq(t, bySDK["exception.message"].AsString(), withStack["exception.message"].AsString())
	must.Eq(t, bySDK["exception.type"].AsString(), withStack["exception.type"].AsString())
	// norca.logger, the stack trace, the message and the type
	must.Eq(t, 4, records[1].AttributesLen(), must.Sprintf("%v", withStack))

	typed := recordAttrs(records[2])
	must.Eq(t, "typed", typed["exception.message"].AsString())
	must.Eq(t, "norca.typed", typed["exception.type"].AsString())
}

func TestLogSink_TraceContext(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	traceID := "0af7651916cd43dd8448eb211c80319c"
	spanID := "b7ad6b7169203331"
	sink.Accept("agent", hclog.Info, "in a span", LogKeyTraceID, traceID, LogKeySpanID, spanID)
	sink.Accept("agent", hclog.Info, "bad ids", LogKeyTraceID, "nope", LogKeySpanID, 7)

	records := mem.all()
	must.Len(t, 2, records)
	r := records[0]
	must.Eq(t, traceID, r.TraceID().String())
	must.Eq(t, spanID, r.SpanID().String())
	attrs := recordAttrs(r)
	_, kept := attrs[LogKeyTraceID]
	must.False(t, kept, must.Sprint("trace_id moved into the trace context"))
	_, kept = attrs[LogKeySpanID]
	must.False(t, kept)

	r = records[1]
	must.False(t, r.TraceID().IsValid())
	attrs = recordAttrs(r)
	must.Eq(t, "nope", attrs[LogKeyTraceID].AsString())
	must.Eq(t, int64(7), attrs[LogKeySpanID].AsInt64())
}

// The fields LogWith adds reach the record's trace context.
func TestLogSink_LogWith(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{Output: io.Discard, Level: hclog.Info})
	logger.RegisterSink(sink)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3},
		SpanID:     trace.SpanID{4, 5, 6},
		TraceFlags: trace.FlagsSampled,
	})
	LogWithSpanContext(logger.Named("worker"), sc).Info("eval processed", "eval_id", "e1")

	records := mem.all()
	must.Len(t, 1, records)
	must.Eq(t, sc.TraceID(), records[0].TraceID())
	must.Eq(t, sc.SpanID(), records[0].SpanID())
	attrs := recordAttrs(records[0])
	must.Eq(t, "worker", attrs[AttrLogger].AsString())
	must.Eq(t, "e1", attrs["eval_id"].AsString())
}

func TestLogSink_OddArguments(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	sink.Accept("agent", hclog.Error, "panic", "a", 1, hclog.CapturedStacktrace("goroutine 1"))
	sink.Accept("agent", hclog.Info, "odd", "a", 1, "lonely")
	sink.Accept("agent", hclog.Info, "key not a string", 42, "v")

	records := mem.all()
	must.Len(t, 3, records)
	must.Eq(t, "goroutine 1", recordAttrs(records[0])[attrExceptionStacktrace].AsString())
	must.Eq(t, "lonely", recordAttrs(records[1])[hclog.MissingKey].AsString())
	must.Eq(t, "v", recordAttrs(records[2])["42"].AsString())
}

// The loop guard: the lines that report a failing log export are not
// exported.
func TestLogSink_DropsOTLPInternal(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Trace)
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{Output: io.Discard, Level: hclog.Trace})
	logger.RegisterSink(sink)

	r := newErrorReporter(logger.Named("otlp"))
	r.log(SignalLogs, errors.New("down"))
	r.log(signalInternal, errors.New("dropped records"))
	r.log(SignalTraces, errors.New("down"))
	logger.Info("ordinary line")

	records := mem.all()
	must.Len(t, 2, records, must.Sprint("the logs and internal lines are dropped"))
	must.Eq(t, "OTLP export failed", records[0].Body().AsString())
	must.Eq(t, "traces", recordAttrs(records[0])["signal"].AsString())
	must.Eq(t, "ordinary line", records[1].Body().AsString())
}

func TestLogSink_Early(t *testing.T) {
	ci.Parallel(t)

	sink := NewLogSink(hclog.Info)
	traceID := "0af7651916cd43dd8448eb211c80319c"
	for i := range earlyLogRecords + 5 {
		sink.Accept("agent", hclog.Info, "setup", "i", i, LogKeyTraceID, traceID)
	}
	sink.Accept("agent", hclog.Debug, "below the level")
	must.Eq(t, 5, sink.EarlyDropped())

	mem := &memProcessor{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(mem))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	sink.Bind(lp.Logger(InstrumentationName))
	records := mem.all()
	must.Len(t, earlyLogRecords, records, must.Sprint("the records of the setup are emitted at the start"))
	must.Eq(t, int64(0), recordAttrs(records[0])["i"].AsInt64())
	must.Eq(t, traceID, records[0].TraceID().String())

	sink.Accept("agent", hclog.Info, "running")
	must.Len(t, earlyLogRecords+1, mem.all())
}

// A sink registered on an intercept logger sees every named logger with its
// implied arguments, at its own level, independent of the logger's level.
func TestLogSink_InterceptLogger(t *testing.T) {
	ci.Parallel(t)

	sink, mem := boundSink(t, hclog.Debug)
	var out syncBuffer
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{Name: "agent", Output: &out, Level: hclog.Info})
	logger.RegisterSink(sink)

	logger.Named("server").With("server_id", "s1").Debug("raft state", "index", uint64(12))
	logger.Trace("too fine")

	records := mem.all()
	must.Len(t, 1, records)
	attrs := recordAttrs(records[0])
	must.Eq(t, "agent.server", attrs[AttrLogger].AsString())
	must.Eq(t, "s1", attrs["server_id"].AsString())
	must.Eq(t, int64(12), attrs["index"].AsInt64())
	must.SliceEmpty(t, out.lines("raft state"), must.Sprint("the log output keeps log_level"))
}

func TestCutUTF8(t *testing.T) {
	ci.Parallel(t)

	s, cut := cutUTF8([]byte("short"), 10)
	must.Eq(t, "short", s)
	must.False(t, cut)

	s, cut = cutUTF8([]byte("aé"), 2) // é is two bytes
	must.Eq(t, "a", s)
	must.True(t, cut)
}

// The cost of the sink per log call: a line below its level, and a line it
// exports (batch processor, no network).
func BenchmarkLogSink_Accept(b *testing.B) {
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(discardExporter{})))
	b.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	sink := NewLogSink(hclog.Info)
	sink.Bind(lp.Logger(InstrumentationName))
	args := []any{"alloc_id", "5f0a1c37-0d1c-4c47-9d6b-2b3a5a8ad0f1", "task", "web", "duration", time.Millisecond}

	b.Run("below level", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink.Accept("agent.client", hclog.Debug, "task event", args...)
		}
	})
	b.Run("exported", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink.Accept("agent.client", hclog.Info, "task event", args...)
		}
	})
}

type discardExporter struct{}

func (discardExporter) Export(context.Context, []sdklog.Record) error { return nil }
func (discardExporter) Shutdown(context.Context) error                { return nil }
func (discardExporter) ForceFlush(context.Context) error              { return nil }
