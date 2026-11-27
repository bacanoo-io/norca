// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/telemetry/otlptest"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// logsOnly enables the logs signal alone, at level.
func logsOnly(endpoint, level string) *OTLPConfig {
	return &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: endpoint},
		Compression:  CompressionNone,
		Timeout:      "2s",
		Retry:        &RetryConfig{Enabled: pointer.Of(false)},
		Metrics:      &MetricsConfig{Enabled: pointer.Of(false)},
		Traces:       &TracesConfig{Enabled: pointer.Of(false)},
		Logs:         &LogsConfig{Enabled: pointer.Of(true), Level: level},
	}
}

// newAgentProviders creates the providers of c as the agent does: they log
// through the agent's intercept logger, which writes its lines to out and
// has the providers' log sink registered.
func newAgentProviders(t *testing.T, c *OTLPConfig, out *syncBuffer) (*Providers, hclog.InterceptLogger) {
	t.Helper()
	s, err := Resolve(c)
	must.NoError(t, err)
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{Name: "agent", Output: out, Level: hclog.Info})
	p, err := New(s, logger, "norca-test/1")
	must.NoError(t, err)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if sink := p.LogSink(); sink != nil {
		logger.RegisterSink(sink)
	}
	return p, logger
}

// receivedLogs returns every log record the server received, with the
// resource and scope of each.
type receivedLog struct {
	resource map[string]*commonpb.AnyValue
	scope    string
	record   *logspb.LogRecord
}

func receivedLogs(srv *otlptest.Server) []receivedLog {
	var out []receivedLog
	for _, req := range srv.Logs() {
		for _, rl := range req.Request.GetResourceLogs() {
			res := resourceAttrs(rl.GetResource())
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					out = append(out, receivedLog{resource: res, scope: sl.GetScope().GetName(), record: lr})
				}
			}
		}
	}
	return out
}

func logAttrs(lr *logspb.LogRecord) map[string]*commonpb.AnyValue {
	out := map[string]*commonpb.AnyValue{}
	for _, kv := range lr.GetAttributes() {
		out[kv.GetKey()] = kv.GetValue()
	}
	return out
}

func flush(t *testing.T, p *Providers) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return p.ForceFlush(ctx)
}

// The acceptance of phase 5: the log records arrive with severity,
// norca.logger, typed attributes and the trace context of the span they
// were written under; the resource is the agent's.
func TestProviders_LogsExport(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	c := logsOnly(srv.Addr, "debug")
	c.Traces = sampled()
	var out syncBuffer
	p, logger := newAgentProviders(t, c, &out)
	logger.Info("before the start", "phase", "setup")
	must.NoError(t, p.Start(testIdentity))
	must.NoError(t, p.Identify(testIdentity))

	ctx, span := p.TracerProvider().Tracer("test").Start(t.Context(), "Job.Register")
	LogWith(logger.Named("worker"), ctx).Debug("eval processed", "eval_id", "e1", "count", 2,
		"duration", 250*time.Millisecond, "error", errors.New("plan rejected"))
	span.End()
	logger.Trace("below the level")
	must.NoError(t, flush(t, p))

	logs := receivedLogs(srv)
	must.Len(t, 2, logs)
	early, inSpan := logs[0], logs[1]

	must.Eq(t, "before the start", early.record.GetBody().GetStringValue())
	must.Eq(t, "agent", logAttrs(early.record)[AttrLogger].GetStringValue())
	must.Eq(t, "INFO", early.record.GetSeverityText())

	r := inSpan.record
	must.Eq(t, "eval processed", r.GetBody().GetStringValue())
	must.Eq(t, logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, r.GetSeverityNumber())
	must.Eq(t, "DEBUG", r.GetSeverityText())
	attrs := logAttrs(r)
	must.Eq(t, "agent.worker", attrs[AttrLogger].GetStringValue())
	must.Eq(t, "e1", attrs["eval_id"].GetStringValue())
	must.Eq(t, int64(2), attrs["count"].GetIntValue())
	must.Eq(t, 250.0, attrs["duration"].GetDoubleValue())
	must.Eq(t, "plan rejected", attrs["exception.message"].GetStringValue())
	must.Eq(t, InstrumentationName, inSpan.scope)

	// the trace context of the span, which went out as a span too
	must.Eq(t, span.SpanContext().TraceID().String(), hex.EncodeToString(r.GetTraceId()))
	must.Eq(t, span.SpanContext().SpanID().String(), hex.EncodeToString(r.GetSpanId()))
	must.True(t, srv.Wait(5*time.Second, func(s *otlptest.Server) bool { return len(s.Traces()) > 0 }))
	exported := srv.Traces()[0].Request.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	must.Eq(t, exported.GetTraceId(), r.GetTraceId())

	must.Eq(t, "norca", inSpan.resource["service.name"].GetStringValue())
	must.Eq(t, "node-1", inSpan.resource["norca.node.id"].GetStringValue())
}

// logs.enabled = false: the providers have no sink to register, and the
// other signals work as before.
func TestProviders_LogsDisabled(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	p, _ := startProviders(t, &OTLPConfig{TargetConfig: TargetConfig{Endpoint: srv.Addr}})
	must.Nil(t, p.LogSink())
	must.Nil(t, p.EventExporter())

	p, _ = startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: srv.Addr},
		Logs:         &LogsConfig{Enabled: pointer.Of(false), Level: "trace"},
	})
	must.Nil(t, p.LogSink())
	must.NoError(t, emit(t, p))
	must.SliceEmpty(t, srv.Logs())

	var none *Providers
	must.Nil(t, none.LogSink())
	must.Nil(t, none.EventExporter())
}

// The loop guard: while the log export fails, the agent's log output grows
// by one rate-limited warning only, and that warning is never exported.
// Not parallel: the OpenTelemetry error handler is global.
func TestProviders_LogsLoopGuard(t *testing.T) {
	srv := otlptest.Start(t, otlptest.Options{})
	srv.Fail(status.Error(codes.Unavailable, "collector down"))
	var out syncBuffer
	p, logger := newAgentProviders(t, logsOnly(srv.Addr, "trace"), &out)
	must.NoError(t, p.Start(testIdentity))
	must.NoError(t, p.Identify(testIdentity))

	const lines = 20
	for i := range lines {
		logger.Info("work", "i", i)
		_ = flush(t, p)
	}
	must.Greater(t, 0, srv.Failed())
	must.Len(t, 1, out.lines("OTLP export failed"), must.Sprint("one warning per minute"))
	must.StrContains(t, out.lines("OTLP export failed")[0], LogKeyOTLPInternal+"=true")
	must.Len(t, lines, out.lines("agent: work:"))
	total := len(out.lines("agent"))
	must.Eq(t, lines+1, total, must.Sprint("no other line was added"))

	// the receiver is back: the lines after it go out, the warning never
	srv.Fail(nil)
	logger.Info("recovered")
	must.NoError(t, flush(t, p))
	for _, l := range receivedLogs(srv) {
		must.NotEq(t, "OTLP export failed", l.record.GetBody().GetStringValue())
	}
	var bodies []string
	for _, l := range receivedLogs(srv) {
		bodies = append(bodies, l.record.GetBody().GetStringValue())
	}
	must.SliceContains(t, bodies, "recovered")
}

func TestProviders_LogsReloadLevel(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	c := logsOnly(srv.Addr, "info")
	p, out := startProviders(t, c)
	must.Eq(t, hclog.Info, p.LogSink().Level())

	c2 := logsOnly(srv.Addr, "debug")
	must.NoError(t, p.Reload(c2))
	must.Eq(t, hclog.Debug, p.LogSink().Level())
	must.SliceEmpty(t, out.lines("restart"), must.Sprint("logs.level is reloaded"))

	// the event export and the signal itself need a restart
	c3 := logsOnly(srv.Addr, "debug")
	c3.Logs.Events = &EventsConfig{Enabled: pointer.Of(true)}
	must.NoError(t, p.Reload(c3))
	must.Len(t, 1, out.lines("restart"))
	must.Nil(t, p.EventExporter())
}

// The shutdown flushes the logs last and completes within its deadline with
// the collector down, all signals and the event export on.
func TestProviders_LogsShutdownCollectorDown(t *testing.T) {
	ci.Parallel(t)

	var out syncBuffer
	p, logger := newAgentProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: otlptest.ClosedAddr(t)},
		Traces:       sampled(),
		Logs:         &LogsConfig{Enabled: pointer.Of(true), Events: &EventsConfig{Enabled: pointer.Of(true)}},
	}, &out)
	must.NoError(t, p.Start(testIdentity))
	must.NoError(t, p.Identify(testIdentity))
	for i := range 100 {
		logger.Info("pending", "i", i)
		_, span := p.TracerProvider().Tracer("test").Start(t.Context(), "pending")
		span.End()
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	_ = p.Shutdown(ctx)
	must.Less(t, ShutdownTimeout+time.Second, time.Since(start))

	// the sink is closed: a line after the shutdown is not kept anywhere
	logger.Info("after the shutdown")
	must.ErrorIs(t, p.EventExporter().Export(t.Context(), []Event{{Topic: "Job"}}), errNotRunning, must.Sprint("the providers are stopped"))
}

// OTEL_RESOURCE_ATTRIBUTES and OTEL_EXPORTER_OTLP_LOGS_* do not change the
// log export. Not parallel: it sets environment variables.
func TestProviders_LogsIgnoreOTELEnvironment(t *testing.T) {
	srv := otlptest.Start(t, otlptest.Options{})
	decoy := otlptest.Start(t, otlptest.Options{})
	for k, v := range map[string]string{
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":            "http://" + decoy.Addr,
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":             "x-env=leak",
		"OTEL_RESOURCE_ATTRIBUTES":                    "leak.key=1",
		"OTEL_SERVICE_NAME":                           "other",
		"OTEL_BLRP_MAX_EXPORT_BATCH_SIZE":             "1",
		"OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT":        "0",
		"OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT": "1",
	} {
		t.Setenv(k, v)
	}
	var out syncBuffer
	p, logger := newAgentProviders(t, logsOnly(srv.Addr, "info"), &out)
	must.NoError(t, p.Start(testIdentity))
	must.NoError(t, p.Identify(testIdentity))
	logger.Info("one", "key", "value")
	logger.Info("two")
	must.NoError(t, flush(t, p))

	must.SliceEmpty(t, decoy.Logs())
	reqs := srv.Logs()
	must.Len(t, 1, reqs, must.Sprint("OTEL_BLRP_MAX_EXPORT_BATCH_SIZE has no effect"))
	must.Eq(t, "", reqs[0].Header("x-env"))
	logs := receivedLogs(srv)
	must.Len(t, 2, logs)
	must.Eq(t, "norca", logs[0].resource["service.name"].GetStringValue())
	_, leaked := logs[0].resource["leak.key"]
	must.False(t, leaked)
	must.Eq(t, "one", logs[0].record.GetBody().GetStringValue(), must.Sprint("no value limit from the environment"))
	must.Eq(t, "value", logAttrs(logs[0].record)["key"].GetStringValue(), must.Sprint("no attribute limit from the environment"))
}

func TestProviders_EventExport(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	c := logsOnly(srv.Addr, "info")
	c.Logs.Events = &EventsConfig{Enabled: pointer.Of(true), Topics: []string{"Job", "Secret"}}
	s, err := Resolve(c)
	must.NoError(t, err)
	var out syncBuffer
	p, err := New(s, testLogger(&out), "norca-test/1")
	must.NoError(t, err)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	exp := p.EventExporter()
	must.NotNil(t, exp)
	must.Eq(t, []string{"Job", "Secret"}, exp.Topics())
	must.ErrorIs(t, exp.Export(t.Context(), []Event{{Topic: "Job"}}), errNotRunning)
	must.NoError(t, p.Start(testIdentity))
	must.NoError(t, p.Identify(testIdentity))

	stored := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	big := `{"Job":{"ID":"` + strings.Repeat("é", MaxEventBody) + `"}}`
	must.NoError(t, exp.Export(t.Context(), []Event{
		{Topic: "Job", Type: "JobRegistered", Key: "web", Namespace: "default", Index: 42, Time: stored, Payload: []byte(`{"Job":{"ID":"web"}}`)},
		{Topic: "Node", Type: "NodeRegistration", Key: "n1", Index: 43, Time: stored, Payload: []byte(big)},
	}))
	must.NoError(t, exp.Export(t.Context(), nil))

	reqs := srv.Logs()
	must.Len(t, 1, reqs, must.Sprint("one request per Export, nothing queued"))
	logs := receivedLogs(srv)
	must.Len(t, 2, logs)
	job := logs[0]
	must.Eq(t, EventsScope, job.scope)
	must.Eq(t, `{"Job":{"ID":"web"}}`, job.record.GetBody().GetStringValue())
	must.Eq(t, uint64(stored.UnixNano()), job.record.GetTimeUnixNano())
	must.Eq(t, logspb.SeverityNumber_SEVERITY_NUMBER_INFO, job.record.GetSeverityNumber())
	attrs := logAttrs(job.record)
	must.Eq(t, "Job", attrs[AttrEventTopic].GetStringValue())
	must.Eq(t, "JobRegistered", attrs[AttrEventType].GetStringValue())
	must.Eq(t, "web", attrs[AttrEventKey].GetStringValue())
	must.Eq(t, "default", attrs[string(AttrNamespace)].GetStringValue())
	must.Eq(t, int64(42), attrs[AttrEventIndex].GetIntValue())
	_, truncated := attrs[AttrEventTruncated]
	must.False(t, truncated)
	must.Eq(t, "node-1", job.resource["norca.node.id"].GetStringValue())

	node := logs[1]
	must.Eq(t, MaxEventBody, len(node.record.GetBody().GetStringValue()))
	attrs = logAttrs(node.record)
	must.True(t, attrs[AttrEventTruncated].GetBoolValue())
	_, hasNamespace := attrs[string(AttrNamespace)]
	must.False(t, hasNamespace)

	// a failing receiver: the error comes back, nothing is kept
	srv.Fail(status.Error(codes.Unavailable, "down"))
	must.Error(t, exp.Export(t.Context(), []Event{{Topic: "Job", Payload: []byte("{}")}}))
	srv.Fail(nil)
	must.NoError(t, exp.Export(t.Context(), []Event{{Topic: "Job", Payload: []byte("{}")}}))
	must.Len(t, 2, srv.Logs())
	must.Len(t, 1, out.lines("signal=events"))
}

func TestProtoResource(t *testing.T) {
	ci.Parallel(t)

	res := resource.NewSchemaless(
		attribute.String("s", "v"),
		attribute.Bool("b", true),
		attribute.Int64("i", 7),
		attribute.Float64("f", 1.5),
		attribute.StringSlice("ss", []string{"server", "client"}),
		attribute.BoolSlice("bs", []bool{true}),
		attribute.Int64Slice("is", []int64{1, 2}),
		attribute.Float64Slice("fs", []float64{0.5}),
	)
	attrs := resourceAttrs(protoResource(res))
	must.Eq(t, "v", attrs["s"].GetStringValue())
	must.True(t, attrs["b"].GetBoolValue())
	must.Eq(t, int64(7), attrs["i"].GetIntValue())
	must.Eq(t, 1.5, attrs["f"].GetDoubleValue())
	must.Eq(t, "client", attrs["ss"].GetArrayValue().GetValues()[1].GetStringValue())
	must.True(t, attrs["bs"].GetArrayValue().GetValues()[0].GetBoolValue())
	must.Eq(t, int64(2), attrs["is"].GetArrayValue().GetValues()[1].GetIntValue())
	must.Eq(t, 0.5, attrs["fs"].GetArrayValue().GetValues()[0].GetDoubleValue())
}
