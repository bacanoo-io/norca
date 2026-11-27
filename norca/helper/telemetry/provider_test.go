// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/telemetry/otlptest"
	metrics "github.com/hashicorp/go-metrics"
	"github.com/shoenig/test/must"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

var testIdentity = Identity{
	Version:  "0.3.0",
	HostName: "host-a",
	Region:   "global",
	NodeID:   "node-1",
	ServerID: "server-1",
}

// startProviders creates and starts the providers of c; they are shut down
// at the end of the test.
func startProviders(t *testing.T, c *OTLPConfig) (*Providers, *syncBuffer) {
	t.Helper()
	s, err := Resolve(c)
	must.NoError(t, err)
	var out syncBuffer
	p, err := New(s, testLogger(&out), "norca-test/1")
	must.NoError(t, err)
	must.NoError(t, p.Start(testIdentity))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p, &out
}

// emit creates one sampled span and one counter value and flushes them.
func emit(t *testing.T, p *Providers) error {
	t.Helper()
	_, span := p.TracerProvider().Tracer("test").Start(t.Context(), "test.span")
	span.End()
	counter, err := p.MeterProvider().Meter("test").Int64Counter("test.counter")
	must.NoError(t, err)
	counter.Add(t.Context(), 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return p.ForceFlush(ctx)
}

func resourceAttrs(res *resourcepb.Resource) map[string]*commonpb.AnyValue {
	out := map[string]*commonpb.AnyValue{}
	for _, kv := range res.GetAttributes() {
		out[kv.GetKey()] = kv.GetValue()
	}
	return out
}

func sampled() *TracesConfig {
	return &TracesConfig{SampleRatio: pointer.Of(1.0)}
}

func TestProviders_Plaintext_HeadersAndGzip(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	headersFile := writeHeadersFile(t, "x-tenant=team-a\n")
	p, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{
			Endpoint:    srv.Addr,
			Headers:     map[string]string{"Authorization": "Bearer inline"},
			HeadersFile: headersFile,
		},
		Traces: sampled(),
	})
	must.NoError(t, emit(t, p))

	traces := srv.Traces()
	must.Len(t, 1, traces)
	tr := traces[0]
	must.Eq(t, "Bearer inline", tr.Header("authorization"))
	must.Eq(t, "team-a", tr.Header("x-tenant"))
	must.StrContains(t, tr.Header("user-agent"), "norca-test/1")
	must.Eq(t, "gzip", tr.Compression)
	must.Eq(t, 0, tr.ClientCertificates)
	rs := tr.Request.GetResourceSpans()[0]
	must.Eq(t, "test.span", rs.GetScopeSpans()[0].GetSpans()[0].GetName())
	attrs := resourceAttrs(rs.GetResource())
	must.Eq(t, "norca", attrs["service.name"].GetStringValue())
	must.Eq(t, "node-1", attrs["norca.node.id"].GetStringValue())
	must.Eq(t, "server-1", attrs["norca.server.id"].GetStringValue())

	ms := srv.Metrics()
	must.SliceNotEmpty(t, ms)
	m := ms[len(ms)-1]
	must.Eq(t, "Bearer inline", m.Header("authorization"))
	must.Eq(t, "gzip", m.Compression)
	rm := m.Request.GetResourceMetrics()[0]
	must.Eq(t, "norca", resourceAttrs(rm.GetResource())["service.name"].GetStringValue())
	must.Eq(t, "test.counter", rm.GetScopeMetrics()[0].GetMetrics()[0].GetName())
}

// With nothing emitted the periodic reader still exports the resource.
func TestProviders_EmptyMetricsExport(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	p, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: srv.Addr},
		Compression:  CompressionNone,
	})
	must.NoError(t, p.ForceFlush(t.Context()))
	ms := srv.Metrics()
	must.Len(t, 1, ms)
	must.Eq(t, "", ms[0].Compression)
	rm := ms[0].Request.GetResourceMetrics()[0]
	must.SliceEmpty(t, rm.GetScopeMetrics())
	must.Eq(t, "global", resourceAttrs(rm.GetResource())["norca.region"].GetStringValue())
}

// The go-metrics bridge exists with the metrics export only; it keeps what
// it gets before Start and exports through the providers after it.
func TestProviders_MetricSink(t *testing.T) {
	ci.Parallel(t)

	var none *Providers
	must.Nil(t, none.MetricSink())

	s, err := Resolve(&OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: otlptest.ClosedAddr(t)},
		Metrics:      &MetricsConfig{Enabled: pointer.Of(false)},
	})
	must.NoError(t, err)
	off, err := New(s, testLogger(&syncBuffer{}), "ua")
	must.NoError(t, err)
	must.Nil(t, off.MetricSink())
	must.NoError(t, off.Shutdown(t.Context()))

	srv := otlptest.Start(t, otlptest.Options{})
	s, err = Resolve(&OTLPConfig{TargetConfig: TargetConfig{Endpoint: srv.Addr}})
	must.NoError(t, err)
	p, err := New(s, testLogger(&syncBuffer{}), "ua")
	must.NoError(t, err)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	sink := p.MetricSink()
	must.NotNil(t, sink)

	sink.SetGauge([]string{"norca", "runtime", "num_goroutines"}, 42)
	must.NoError(t, p.Start(testIdentity))
	sink.AddSample([]string{"norca", "raft", "apply"}, 3)
	must.NoError(t, p.ForceFlush(t.Context()))

	units := map[string]string{}
	for _, m := range srv.Metrics() {
		for _, rm := range m.Request.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, metric := range sm.GetMetrics() {
					units[metric.GetName()] = metric.GetUnit()
				}
			}
		}
	}
	must.Eq(t, map[string]string{"norca.runtime.num_goroutines": "", "norca.raft.apply": "ms"}, units)
}

func TestProviders_TLS(t *testing.T) {
	ci.Parallel(t)

	certs := otlptest.NewCerts(t)
	srv := otlptest.Start(t, otlptest.Options{TLS: certs.ServerTLS(false)})
	p, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{
			Endpoint: srv.Addr,
			Headers:  map[string]string{"authorization": "Basic dXNlcjpwYXNz"},
			TLS:      &TLSConfig{CAFile: certs.CAFile, ServerName: otlptest.ServerName},
		},
		Traces: sampled(),
	})
	must.NoError(t, emit(t, p))
	traces := srv.Traces()
	must.Len(t, 1, traces)
	must.Eq(t, "Basic dXNlcjpwYXNz", traces[0].Header("authorization"))
	must.Eq(t, "gzip", traces[0].Compression)
	must.Eq(t, 0, traces[0].ClientCertificates)
	must.SliceNotEmpty(t, srv.Metrics())
}

func TestProviders_TLS_UnknownCA(t *testing.T) {
	ci.Parallel(t)

	certs := otlptest.NewCerts(t)
	other := otlptest.NewCerts(t)
	srv := otlptest.Start(t, otlptest.Options{TLS: certs.ServerTLS(false)})
	p, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{
			Endpoint: srv.Addr,
			TLS:      &TLSConfig{CAFile: other.CAFile},
		},
		Timeout: "2s",
		Retry:   &RetryConfig{Enabled: pointer.Of(false)},
		Traces:  sampled(),
	})
	must.Error(t, emit(t, p))
	must.SliceEmpty(t, srv.Traces())
}

func TestProviders_MTLS(t *testing.T) {
	ci.Parallel(t)

	certs := otlptest.NewCerts(t)
	srv := otlptest.Start(t, otlptest.Options{TLS: certs.ServerTLS(true)})

	t.Run("with a client certificate", func(t *testing.T) {
		p, _ := startProviders(t, &OTLPConfig{
			TargetConfig: TargetConfig{
				Endpoint: srv.Addr,
				Headers:  map[string]string{"x-api-key": "k"},
				TLS: &TLSConfig{
					CAFile:   certs.CAFile,
					CertFile: certs.ClientCertFile,
					KeyFile:  certs.ClientKeyFile,
				},
			},
			Traces: sampled(),
		})
		must.NoError(t, emit(t, p))
		traces := srv.Traces()
		must.Len(t, 1, traces)
		must.Eq(t, 1, traces[0].ClientCertificates)
		must.Eq(t, "k", traces[0].Header("x-api-key"))
		must.Eq(t, "gzip", traces[0].Compression)
	})

	t.Run("without one", func(t *testing.T) {
		p, _ := startProviders(t, &OTLPConfig{
			TargetConfig: TargetConfig{Endpoint: srv.Addr, TLS: &TLSConfig{CAFile: certs.CAFile}},
			Timeout:      "2s",
			Retry:        &RetryConfig{Enabled: pointer.Of(false)},
			Traces:       sampled(),
		})
		must.Error(t, emit(t, p))
	})
}

func TestProviders_SignalTargets(t *testing.T) {
	ci.Parallel(t)

	central := otlptest.Start(t, otlptest.Options{})
	tracesOnly := otlptest.Start(t, otlptest.Options{})
	p, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: central.Addr, Headers: map[string]string{"x-backend": "central"}},
		Traces: &TracesConfig{
			TargetConfig: TargetConfig{Endpoint: tracesOnly.Addr, Headers: map[string]string{"x-backend": "traces"}},
			SampleRatio:  pointer.Of(1.0),
		},
	})
	must.NoError(t, emit(t, p))
	must.SliceEmpty(t, central.Traces())
	must.SliceNotEmpty(t, central.Metrics())
	must.Eq(t, "central", central.Metrics()[0].Header("x-backend"))
	must.Len(t, 1, tracesOnly.Traces())
	must.Eq(t, "traces", tracesOnly.Traces()[0].Header("x-backend"))
	must.SliceEmpty(t, tracesOnly.Metrics())
}

func TestProviders_ReloadHeadersFile(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	headersFile := writeHeadersFile(t, "authorization=Bearer one\n")
	c := &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: srv.Addr, HeadersFile: headersFile},
		Traces:       sampled(),
	}
	p, out := startProviders(t, c)
	must.NoError(t, emit(t, p))
	must.Eq(t, "Bearer one", srv.Traces()[0].Header("authorization"))

	must.NoError(t, os.WriteFile(headersFile, []byte("authorization=Bearer two\n"), 0o600))
	must.NoError(t, p.Reload(c.Copy()))
	must.NoError(t, emit(t, p))
	traces := srv.Traces()
	must.Len(t, 2, traces)
	must.Eq(t, "Bearer two", traces[1].Header("authorization"))
	must.SliceEmpty(t, out.lines("restart"), must.Sprint("the headers file is reloadable"))

	// a broken file keeps the headers
	must.NoError(t, os.WriteFile(headersFile, []byte("broken\n"), 0o600))
	must.Error(t, p.Reload(c.Copy()))
	must.NoError(t, emit(t, p))
	must.Eq(t, "Bearer two", srv.Traces()[2].Header("authorization"))

	// other changes need a restart, which is logged
	must.NoError(t, os.WriteFile(headersFile, []byte("authorization=Bearer three\n"), 0o600))
	changed := c.Copy()
	changed.Timeout = "3s"
	must.NoError(t, p.Reload(changed))
	must.Len(t, 1, out.lines("restart"))

	// an invalid block is refused
	invalid := c.Copy()
	invalid.Compression = "zstd"
	must.ErrorContains(t, p.Reload(invalid), "compression")
}

func TestProviders_ReloadSampleRatio(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	c := &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: srv.Addr},
		Traces:       &TracesConfig{SampleRatio: pointer.Of(0.0)},
	}
	p, out := startProviders(t, c)
	tracer := p.TracerProvider().Tracer("test")
	_, span := tracer.Start(t.Context(), "dropped")
	must.False(t, span.SpanContext().IsSampled())
	must.True(t, span.SpanContext().HasTraceID(), must.Sprint("trace IDs exist for unsampled requests"))
	span.End()

	changed := c.Copy()
	changed.Traces.SampleRatio = pointer.Of(1.0)
	must.NoError(t, p.Reload(changed))
	must.SliceEmpty(t, out.lines("restart"))
	_, span = tracer.Start(t.Context(), "kept")
	must.True(t, span.SpanContext().IsSampled())
	span.End()
}

// The agent configuration is the only source: none of the OTEL_* variables
// the exporters and the SDK read changes the export. Not parallel: it sets
// environment variables.
func TestProviders_IgnoresOTELEnvironment(t *testing.T) {
	srv := otlptest.Start(t, otlptest.Options{})
	decoy := otlptest.Start(t, otlptest.Options{})
	for k, v := range map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":                              "http://" + decoy.Addr,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":                       "http://" + decoy.Addr,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":                      "http://" + decoy.Addr,
		"OTEL_EXPORTER_OTLP_INSECURE":                              "false",
		"OTEL_EXPORTER_OTLP_CERTIFICATE":                           filepath.Join(t.TempDir(), "missing.pem"),
		"OTEL_EXPORTER_OTLP_HEADERS":                               "x-env=leak",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":                        "x-env=leak",
		"OTEL_EXPORTER_OTLP_COMPRESSION":                           "gzip",
		"OTEL_EXPORTER_OTLP_TIMEOUT":                               "1",
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE":        "delta",
		"OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION": "base2_exponential_bucket_histogram",
		"OTEL_RESOURCE_ATTRIBUTES":                                 "leak.key=1",
		"OTEL_SERVICE_NAME":                                        "other",
		"OTEL_TRACES_SAMPLER":                                      "always_off",
		"OTEL_METRIC_EXPORT_INTERVAL":                              "1",
		"OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT":                          "0",
	} {
		t.Setenv(k, v)
	}

	p, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: srv.Addr},
		Compression:  CompressionNone,
		Metrics:      &MetricsConfig{Interval: "1h"},
		Traces:       sampled(),
	})
	tracer := p.TracerProvider().Tracer("test")
	_, span := tracer.Start(t.Context(), "env.span")
	must.True(t, span.SpanContext().IsSampled(), must.Sprint("OTEL_TRACES_SAMPLER has no effect"))
	span.End()
	must.NoError(t, emit(t, p))

	must.SliceEmpty(t, decoy.Traces())
	must.SliceEmpty(t, decoy.Metrics())
	traces := srv.Traces()
	must.SliceNotEmpty(t, traces)
	for _, tr := range traces {
		must.Eq(t, "", tr.Header("x-env"))
		must.Eq(t, "", tr.Compression)
		attrs := resourceAttrs(tr.Request.GetResourceSpans()[0].GetResource())
		must.Eq(t, "norca", attrs["service.name"].GetStringValue())
		_, leaked := attrs["leak.key"]
		must.False(t, leaked)
	}
	ms := srv.Metrics()
	must.Len(t, 1, ms, must.Sprint("OTEL_METRIC_EXPORT_INTERVAL has no effect"))
	m := ms[0]
	must.Eq(t, "", m.Header("x-env"))
	must.Eq(t, "", m.Compression)
	_, leaked := resourceAttrs(m.Request.GetResourceMetrics()[0].GetResource())["leak.key"]
	must.False(t, leaked)
	sum := m.Request.GetResourceMetrics()[0].GetScopeMetrics()[0].GetMetrics()[0].GetSum()
	must.Eq(t, "AGGREGATION_TEMPORALITY_CUMULATIVE", sum.GetAggregationTemporality().String())
}

// An absent collector neither delays the startup nor floods the log: one
// warning per signal per minute, every error counted in go-metrics. Not
// parallel: it installs a global go-metrics sink and counts what reaches it.
func TestProviders_ClosedPort(t *testing.T) {
	inm := metrics.NewInmemSink(time.Minute, time.Minute)
	sink, err := metrics.NewGlobal(metrics.DefaultConfig("norca"), inm)
	must.NoError(t, err)
	t.Cleanup(sink.Shutdown)

	s, err := Resolve(&OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: otlptest.ClosedAddr(t)},
		Timeout:      "1s",
		Retry:        &RetryConfig{Enabled: pointer.Of(false)},
		Traces:       sampled(),
	})
	must.NoError(t, err)
	var out syncBuffer
	start := time.Now()
	p, err := New(s, testLogger(&out), "norca-test/1")
	must.NoError(t, err)
	must.NoError(t, p.Start(testIdentity))
	must.Less(t, time.Second, time.Since(start), must.Sprint("no dial at startup"))

	for range 3 {
		must.Error(t, emit(t, p))
	}
	must.Len(t, 1, out.lines("signal=traces"))
	must.Len(t, 1, out.lines("signal=metrics"))

	counted := map[string]int{}
	for _, interval := range inm.Data() {
		interval.RLock()
		for key, c := range interval.Counters {
			if strings.HasPrefix(key, "norca.agent.otlp.export_errors;") {
				counted[key] += c.Count
			}
		}
		interval.RUnlock()
	}
	must.Eq(t, 3, counted["norca.agent.otlp.export_errors;signal=traces"])
	must.GreaterEq(t, 3, counted["norca.agent.otlp.export_errors;signal=metrics"])

	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	_ = p.Shutdown(ctx)
	must.Len(t, 1, out.lines("signal=metrics"), must.Sprint("the final flush does not log"))
}

// The shutdown completes within its deadline with the collector down and
// data pending, with the default timeout (10s) and retry (1m).
func TestProviders_ShutdownCollectorDown(t *testing.T) {
	ci.Parallel(t)

	s, err := Resolve(&OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: otlptest.ClosedAddr(t)},
		Traces:       sampled(),
	})
	must.NoError(t, err)
	var out syncBuffer
	p, err := New(s, testLogger(&out), "norca-test/1")
	must.NoError(t, err)
	must.NoError(t, p.Start(testIdentity))
	for i := range 10 {
		_, span := p.TracerProvider().Tracer("test").Start(t.Context(), "pending")
		span.End()
		counter, err := p.MeterProvider().Meter("test").Int64Counter("pending")
		must.NoError(t, err)
		counter.Add(t.Context(), int64(i))
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	_ = p.Shutdown(ctx)
	elapsed := time.Since(start)
	must.Less(t, ShutdownTimeout+time.Second, elapsed)
	must.SliceEmpty(t, out.lines("OTLP export failed"))

	// idempotent
	must.NoError(t, p.Shutdown(context.Background()))
}

func TestProviders_Lifecycle(t *testing.T) {
	ci.Parallel(t)

	t.Run("nil", func(t *testing.T) {
		var p *Providers
		must.NoError(t, p.Start(testIdentity))
		must.NoError(t, p.ForceFlush(t.Context()))
		must.NoError(t, p.Reload(nil))
		must.ErrorContains(t, p.Reload(&OTLPConfig{}), "restart")
		must.NoError(t, p.Shutdown(t.Context()))
		_, span := p.TracerProvider().Tracer("t").Start(t.Context(), "s")
		must.False(t, span.SpanContext().IsValid())

		created, err := New(nil, testLogger(&syncBuffer{}), "ua")
		must.NoError(t, err)
		must.Nil(t, created)
	})

	t.Run("shutdown before start", func(t *testing.T) {
		s, err := Resolve(&OTLPConfig{TargetConfig: TargetConfig{Endpoint: otlptest.ClosedAddr(t)}})
		must.NoError(t, err)
		p, err := New(s, testLogger(&syncBuffer{}), "ua")
		must.NoError(t, err)
		must.NoError(t, p.Shutdown(t.Context()))
		must.Error(t, p.Start(testIdentity))
	})

	t.Run("signals off", func(t *testing.T) {
		srv := otlptest.Start(t, otlptest.Options{})
		p, _ := startProviders(t, &OTLPConfig{
			TargetConfig: TargetConfig{Endpoint: srv.Addr},
			Metrics:      &MetricsConfig{Enabled: pointer.Of(false)},
			Traces:       &TracesConfig{Enabled: pointer.Of(false), SampleRatio: pointer.Of(1.0)},
		})
		must.Error(t, p.Start(testIdentity), must.Sprint("started once"))
		_, span := p.TracerProvider().Tracer("t").Start(t.Context(), "s")
		must.True(t, span.SpanContext().IsValid(), must.Sprint("a trace ID exists without the traces export"))
		span.End()
		must.NoError(t, emit(t, p))
		must.SliceEmpty(t, srv.Traces())
		must.SliceEmpty(t, srv.Metrics())
	})

	t.Run("missing headers file fails New", func(t *testing.T) {
		s, err := Resolve(&OTLPConfig{TargetConfig: TargetConfig{HeadersFile: filepath.Join(t.TempDir(), "missing")}})
		must.NoError(t, err)
		_, err = New(s, testLogger(&syncBuffer{}), "ua")
		must.ErrorContains(t, err, "telemetry.otlp.traces")
	})

	t.Run("missing ca fails New", func(t *testing.T) {
		s, err := Resolve(&OTLPConfig{TargetConfig: TargetConfig{
			Endpoint: "otlp.example.com:4317",
			TLS:      &TLSConfig{CAFile: filepath.Join(t.TempDir(), "missing.pem")},
		}})
		must.NoError(t, err)
		_, err = New(s, testLogger(&syncBuffer{}), "ua")
		must.ErrorContains(t, err, "tls.ca_file")
	})

	t.Run("insecure remote warns at startup", func(t *testing.T) {
		s, err := Resolve(&OTLPConfig{TargetConfig: TargetConfig{Endpoint: "10.255.255.1:4317", Insecure: pointer.Of(true)}})
		must.NoError(t, err)
		var out syncBuffer
		p, err := New(s, testLogger(&out), "ua")
		must.NoError(t, err)
		must.NoError(t, p.Shutdown(t.Context()))
		must.Len(t, 2, out.lines("without TLS"), must.Sprint("traces and metrics; logs are off"))
	})
}

// The signals with the same target share one connection.
func TestProviders_SharedConnection(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	p, _ := startProviders(t, &OTLPConfig{TargetConfig: TargetConfig{Endpoint: srv.Addr}})
	must.Len(t, 1, p.conns.all)
}

func TestSelectors(t *testing.T) {
	ci.Parallel(t)

	delta := temporalitySelector(TemporalityDelta)
	must.Eq(t, "DeltaTemporality", delta(sdkmetric.InstrumentKindCounter).String())
	must.Eq(t, "DeltaTemporality", delta(sdkmetric.InstrumentKindHistogram).String())
	must.Eq(t, "CumulativeTemporality", delta(sdkmetric.InstrumentKindUpDownCounter).String())
	must.Eq(t, "CumulativeTemporality", temporalitySelector(TemporalityCumulative)(sdkmetric.InstrumentKindCounter).String())

	exponential := aggregationSelector(HistogramsExponential)
	_, isExp := exponential(sdkmetric.InstrumentKindHistogram).(sdkmetric.AggregationBase2ExponentialHistogram)
	must.True(t, isExp)
	_, isExplicit := aggregationSelector(HistogramsExplicit)(sdkmetric.InstrumentKindHistogram).(sdkmetric.AggregationExplicitBucketHistogram)
	must.True(t, isExplicit)
}

// The spans of the agent's instrumentation (the HTTP API and the RPC
// server) reach the receiver with the agent's resource, parent and child
// in one trace; without the traces export they are not exported, but the
// trace IDs exist.
func TestProviders_TracingExport(t *testing.T) {
	ci.Parallel(t)

	srv := otlptest.Start(t, otlptest.Options{})
	p, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: srv.Addr},
		Traces:       &TracesConfig{SampleRatio: pointer.Of(0.0)},
	})
	tracing := p.Tracing()
	must.NotNil(t, tracing)

	// the HTTP API calls an RPC with the request's traceparent
	rpcSrv := newTestRPCServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		args := &RPCTestArgs{TraceParent: TraceParentFromContext(r.Context())}
		c := &memCodec{method: "Test.Call", args: args, reply: &RPCTestReply{}}
		must.NoError(t, rpcSrv.ServeRequest(tracing.WrapServerCodec(c)))
	})
	w := request(tracing.HTTPHandler(mux), "PUT", "/v1/jobs", nil)
	traceID := w.Header().Get(TraceIDHeader)
	must.NotEq(t, "", traceID)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	must.NoError(t, p.ForceFlush(ctx))

	names := map[string]*tracepb.Span{}
	for _, tr := range srv.Traces() {
		for _, rs := range tr.Request.GetResourceSpans() {
			attrs := resourceAttrs(rs.GetResource())
			must.Eq(t, "norca", attrs["service.name"].GetStringValue())
			must.Eq(t, "server-1", attrs["norca.server.id"].GetStringValue())
			for _, ss := range rs.GetScopeSpans() {
				must.Eq(t, InstrumentationName, ss.GetScope().GetName())
				for _, s := range ss.GetSpans() {
					names[s.GetName()] = s
				}
			}
		}
	}
	httpSpan, rpcSpan := names["PUT /v1/jobs"], names["Test.Call"]
	must.NotNil(t, httpSpan, must.Sprint("an always-sampled route at ratio 0"))
	must.NotNil(t, rpcSpan)
	must.Eq(t, traceID, hex.EncodeToString(httpSpan.GetTraceId()))
	must.Eq(t, httpSpan.GetTraceId(), rpcSpan.GetTraceId())
	must.Eq(t, httpSpan.GetSpanId(), rpcSpan.GetParentSpanId())

	// traces export off: IDs for the logs, nothing sampled or exported
	off, _ := startProviders(t, &OTLPConfig{
		TargetConfig: TargetConfig{Endpoint: srv.Addr},
		Traces:       &TracesConfig{Enabled: pointer.Of(false)},
	})
	w = request(off.Tracing().HTTPHandler(mux), "PUT", "/v1/jobs", nil)
	must.NotEq(t, "", w.Header().Get(TraceIDHeader))
}
