// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"fmt"
	"reflect"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
)

// connection is the gRPC connection of one or more signals with the same
// target. The agent creates it itself and hands it to the exporters
// (WithGRPCConn), so the endpoint, the transport security and the
// compression are its own: the OTEL_EXPORTER_OTLP_* variables, which the
// exporters read for any option not given, cannot change them.
type connection struct {
	target  Target
	headers *headerCredentials
	conn    *grpc.ClientConn
}

// newConnection reads the files of a target (CA, client certificate,
// headers file) and creates the connection. It does not dial: gRPC connects
// on the first export, so an absent receiver neither blocks nor fails the
// startup, while a missing file does fail it.
func newConnection(s *Settings, t Target, userAgent string) (*connection, error) {
	var transport credentials.TransportCredentials
	if t.Plaintext {
		transport = insecure.NewCredentials()
	} else {
		tc, err := clientTLSConfig(t)
		if err != nil {
			return nil, err
		}
		transport = credentials.NewTLS(tc)
	}
	headers, err := newHeaderCredentials(t)
	if err != nil {
		return nil, err
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(transport),
		grpc.WithPerRPCCredentials(headers),
		grpc.WithUserAgent(userAgent),
	}
	if s.Compression == CompressionGzip {
		opts = append(opts, grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)))
	}
	conn, err := grpc.NewClient(t.Endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("endpoint %s: %w", t.Endpoint, err)
	}
	return &connection{target: t, headers: headers, conn: conn}, nil
}

// connections creates the connections of the enabled signals, one per
// distinct target.
type connections struct {
	settings  *Settings
	userAgent string
	all       []*connection
}

// get returns the connection of a target, creating it on first use.
func (cs *connections) get(t Target) (*connection, error) {
	for _, c := range cs.all {
		if reflect.DeepEqual(c.target, t) {
			return c, nil
		}
	}
	c, err := newConnection(cs.settings, t, cs.userAgent)
	if err != nil {
		return nil, err
	}
	cs.all = append(cs.all, c)
	return c, nil
}

// reload reads the headers files again.
func (cs *connections) reload() error {
	var first error
	for _, c := range cs.all {
		if err := c.headers.reload(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (cs *connections) close() error {
	var first error
	for _, c := range cs.all {
		if err := c.conn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// The exporters get every remaining option explicitly too: an empty header
// map (their own headers would come from OTEL_EXPORTER_OTLP_HEADERS; the
// agent's headers travel as per-RPC credentials of the connection), the
// timeout and the retry.

func traceOptions(s *Settings, c *connection) []otlptracegrpc.Option {
	return []otlptracegrpc.Option{
		otlptracegrpc.WithGRPCConn(c.conn),
		otlptracegrpc.WithHeaders(map[string]string{}),
		otlptracegrpc.WithTimeout(s.Timeout),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled:         s.Retry.Enabled,
			InitialInterval: s.Retry.InitialInterval,
			MaxInterval:     s.Retry.MaxInterval,
			MaxElapsedTime:  s.Retry.MaxElapsed,
		}),
	}
}

func metricOptions(s *Settings, c *connection) []otlpmetricgrpc.Option {
	return []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithGRPCConn(c.conn),
		otlpmetricgrpc.WithHeaders(map[string]string{}),
		otlpmetricgrpc.WithTimeout(s.Timeout),
		otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{
			Enabled:         s.Retry.Enabled,
			InitialInterval: s.Retry.InitialInterval,
			MaxInterval:     s.Retry.MaxInterval,
			MaxElapsedTime:  s.Retry.MaxElapsed,
		}),
		otlpmetricgrpc.WithTemporalitySelector(temporalitySelector(s.Metrics.Temporality)),
		otlpmetricgrpc.WithAggregationSelector(aggregationSelector(s.Metrics.Histograms)),
	}
}

// temporalitySelector maps metrics.temporality to the SDK. "delta" follows
// the delta preference of the OTLP exporter specification: counters and
// histograms delta, up-down counters cumulative.
func temporalitySelector(temporality string) sdkmetric.TemporalitySelector {
	if temporality != TemporalityDelta {
		return sdkmetric.DefaultTemporalitySelector
	}
	return func(kind sdkmetric.InstrumentKind) metricdata.Temporality {
		switch kind {
		case sdkmetric.InstrumentKindCounter,
			sdkmetric.InstrumentKindObservableCounter,
			sdkmetric.InstrumentKindHistogram:
			return metricdata.DeltaTemporality
		default:
			return metricdata.CumulativeTemporality
		}
	}
}

// aggregationSelector maps metrics.histograms to the SDK.
func aggregationSelector(histograms string) sdkmetric.AggregationSelector {
	if histograms != HistogramsExponential {
		return sdkmetric.DefaultAggregationSelector
	}
	return func(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
		if kind == sdkmetric.InstrumentKindHistogram {
			return sdkmetric.AggregationBase2ExponentialHistogram{MaxSize: 160, MaxScale: 20}
		}
		return sdkmetric.DefaultAggregationSelector(kind)
	}
}

// spanExporter pins the agent's resource on every exported span (the SDK
// would add the attributes of OTEL_RESOURCE_ATTRIBUTES to it) and reports
// the export errors of the traces signal.
type spanExporter struct {
	next sdktrace.SpanExporter
	errs *errorReporter

	// resource is set by Start, before any span exists.
	resource *resource.Resource

	// done, set by Start, is told the size of every batch once the export
	// is over, and whether it failed (spanQueue).
	done func(n int, failed bool)
}

var _ sdktrace.SpanExporter = (*spanExporter)(nil)

// pinnedSpan is a span with the agent's resource.
type pinnedSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s pinnedSpan) Resource() *resource.Resource { return s.resource }

func (e *spanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	pinned := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		pinned[i] = pinnedSpan{ReadOnlySpan: s, resource: e.resource}
	}
	err := e.next.ExportSpans(ctx, pinned)
	if e.done != nil {
		e.done(len(spans), err != nil)
	}
	if err != nil {
		e.errs.export(SignalTraces, err)
		return &exportError{signal: SignalTraces, err: err}
	}
	return nil
}

func (e *spanExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}

// metricExporter pins the agent's resource on every export and reports the
// export errors of the metrics signal.
type metricExporter struct {
	sdkmetric.Exporter
	errs *errorReporter

	// resource is set by Start, before the reader runs.
	resource *resource.Resource
}

var _ sdkmetric.Exporter = (*metricExporter)(nil)

func (e *metricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	pinned := *rm
	pinned.Resource = e.resource
	if err := e.Exporter.Export(ctx, &pinned); err != nil {
		e.errs.export(SignalMetrics, err)
		return &exportError{signal: SignalMetrics, err: err}
	}
	return nil
}
