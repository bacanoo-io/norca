// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ShutdownTimeout is the one deadline the providers share at shutdown.
const ShutdownTimeout = 5 * time.Second

// Fixed values of the SDK that would otherwise come from OTEL_* variables.
const (
	// batchTimeout is how long the batch span processor waits for a full
	// batch (the SDK default).
	batchTimeout = 5 * time.Second

	// cardinalityLimit bounds the attribute sets of one instrument (the SDK
	// default; OTEL_GO_X_CARDINALITY_LIMIT would change it).
	cardinalityLimit = 2000
)

// spanLimits are the SDK defaults, given explicitly so the OTEL_SPAN_*
// variables have no effect.
var spanLimits = sdktrace.SpanLimits{
	AttributeValueLengthLimit:   sdktrace.DefaultAttributeValueLengthLimit,
	AttributeCountLimit:         sdktrace.DefaultAttributeCountLimit,
	EventCountLimit:             sdktrace.DefaultEventCountLimit,
	LinkCountLimit:              sdktrace.DefaultLinkCountLimit,
	AttributePerEventCountLimit: sdktrace.DefaultAttributePerEventCountLimit,
	AttributePerLinkCountLimit:  sdktrace.DefaultAttributePerLinkCountLimit,
}

// Providers owns the OpenTelemetry providers of the agent, their exporters
// and their connections. It exists only with an otlp block. The providers
// are not installed as the OpenTelemetry globals, so library
// instrumentation keeps its no-op default; only the W3C Trace Context and
// Baggage propagator is set globally.
//
// The lifecycle has two steps because the resource needs the IDs of the
// running agent while a bad file must fail the startup before the agent
// runs: New (before the agent) reads the files and creates the exporters,
// Start (after the agent) creates the providers. Shutdown works after
// either step.
type Providers struct {
	logger   hclog.Logger
	settings *Settings
	errs     *errorReporter
	conns    *connections
	sampler  *ratioSampler

	// tracing is the trace instrumentation handed to the server, the
	// client and the HTTP API; it exists from New and is bound in Start.
	tracing *Tracing

	spans   *spanExporter
	metrics *metricExporter

	// sink bridges go-metrics to the meter provider (metrics signal on);
	// it exists from New and is bound in Start.
	sink *OTelSink

	mu      sync.Mutex
	started bool
	stopped bool
	tracer  *sdktrace.TracerProvider
	meter   *sdkmetric.MeterProvider
}

// New validates the settings' files and creates the exporters of the
// enabled signals. A nil settings (no otlp block) returns nil providers,
// whose methods are no-ops. userAgent is sent with every export.
func New(s *Settings, logger hclog.Logger, userAgent string) (*Providers, error) {
	if s == nil {
		return nil, nil
	}
	logger = logger.Named("otlp")
	p := &Providers{
		logger:   logger,
		settings: s,
		errs:     newErrorReporter(logger),
		conns:    &connections{settings: s, userAgent: userAgent},
		sampler:  newRatioSampler(s.Traces.SampleRatio),
		tracing:  newTracing(s.Traces),
	}
	if err := p.newExporters(); err != nil {
		if p.spans != nil {
			_ = p.spans.Shutdown(context.Background())
		}
		_ = p.conns.close()
		return nil, err
	}
	for _, w := range s.Warnings {
		logger.Warn("insecure OTLP export", "warning", w)
	}
	return p, nil
}

func (p *Providers) newExporters() error {
	s := p.settings
	if s.Traces.Enabled {
		c, err := p.conns.get(s.Traces.Target)
		if err != nil {
			return fmt.Errorf("telemetry.otlp.traces: %w", err)
		}
		exp, err := otlptracegrpc.New(context.Background(), traceOptions(s, c)...)
		if err != nil {
			return fmt.Errorf("telemetry.otlp.traces: creating the exporter: %w", err)
		}
		p.spans = &spanExporter{next: exp, errs: p.errs}
	}
	if s.Metrics.Enabled {
		c, err := p.conns.get(s.Metrics.Target)
		if err != nil {
			return fmt.Errorf("telemetry.otlp.metrics: %w", err)
		}
		exp, err := otlpmetricgrpc.New(context.Background(), metricOptions(s, c)...)
		if err != nil {
			return fmt.Errorf("telemetry.otlp.metrics: creating the exporter: %w", err)
		}
		p.metrics = &metricExporter{Exporter: exp, errs: p.errs}
		p.sink = NewOTelSink(nil, OTelSinkOptions{Logger: p.logger})
	}
	// The logs signal (phase 5 of the plan) gets its exporter here from
	// s.Logs.Target, in the same way, once the log SDK is linked.
	return nil
}

// Start creates the providers with the resource of the running agent,
// installs the error handler and the global propagator, and starts the
// periodic metric export. It is called once.
func (p *Providers) Start(id Identity) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.stopped {
		return errors.New("telemetry: the providers were started or stopped already")
	}
	res, err := newResource(context.Background(), id, p.settings.ResourceAttributes)
	if err != nil {
		return err
	}
	installErrorHandler(p.errs)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	// A TracerProvider exists with every otlp block, so trace IDs exist
	// (for the logs) even with the traces export off. Without the export
	// nothing is sampled: the spans are non-recording and cost little.
	sampler := sdktrace.NeverSample()
	if p.spans != nil {
		sampler = NewSampler(p.sampler)
	}
	topts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
		sdktrace.WithRawSpanLimits(spanLimits),
	}
	if p.spans != nil {
		p.spans.resource = res
		t := p.settings.Traces
		topts = append(topts, sdktrace.WithSpanProcessor(newSpanQueue(p.spans, t.MaxQueueSize,
			sdktrace.WithMaxExportBatchSize(t.MaxExportBatch),
			sdktrace.WithBatchTimeout(batchTimeout),
			sdktrace.WithExportTimeout(p.settings.Timeout),
		)))
	}
	p.tracer = sdktrace.NewTracerProvider(topts...)
	p.tracing.bind(p.tracer)

	if p.metrics != nil {
		p.metrics.resource = res
		p.meter = sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
			sdkmetric.WithCardinalityLimit(cardinalityLimit),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(p.metrics,
				sdkmetric.WithInterval(p.settings.Metrics.Interval),
				sdkmetric.WithTimeout(p.settings.Timeout),
			)),
		)
		p.sink.Bind(p.meter)
	}
	p.started = true
	return nil
}

// TracerProvider returns the agent's tracer provider, or a no-op one when
// there is none (no otlp block, or not started).
func (p *Providers) TracerProvider() trace.TracerProvider {
	if p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.tracer != nil {
			return p.tracer
		}
	}
	return tracenoop.NewTracerProvider()
}

// Tracing returns the trace instrumentation of the agent, for the server,
// the client and the HTTP API; nil without an otlp block, so they install
// no wrapper. It exists before Start and traces from Start on.
func (p *Providers) Tracing() *Tracing {
	if p == nil {
		return nil
	}
	return p.tracing
}

// MeterProvider returns the agent's meter provider, or a no-op one when the
// metrics export is off.
func (p *Providers) MeterProvider() metric.MeterProvider {
	if p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.meter != nil {
			return p.meter
		}
	}
	return metricnoop.NewMeterProvider()
}

// MetricSink returns the go-metrics sink that bridges the agent's metrics
// to the OTLP export, for the go-metrics fanout; nil when the metrics
// export is off. It exists before Start, which binds it to the meter
// provider, so it can join the fanout when the telemetry is set up.
func (p *Providers) MetricSink() *OTelSink {
	if p == nil {
		return nil
	}
	return p.sink
}

// ForceFlush exports what the providers hold.
func (p *Providers) ForceFlush(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	tracer, meter := p.tracer, p.meter
	p.mu.Unlock()
	var errs []error
	if tracer != nil {
		errs = append(errs, tracer.ForceFlush(ctx))
	}
	if meter != nil {
		errs = append(errs, meter.ForceFlush(ctx))
	}
	return errors.Join(errs...)
}

// Reload applies a reloaded otlp block: the headers files are read again
// and sample_ratio takes effect. Any other change needs a restart, which is
// logged. An error leaves the running settings as they were.
func (p *Providers) Reload(c *OTLPConfig) error {
	if p == nil {
		if c != nil {
			return errors.New("telemetry.otlp: adding the otlp block needs a restart of the agent")
		}
		return nil
	}
	if c == nil {
		p.logger.Warn("removing the otlp block needs a restart of the agent")
		return nil
	}
	s, err := Resolve(c)
	if err != nil {
		return err
	}
	if !sameExceptReloadable(p.settings, s) {
		p.logger.Warn("changes of the otlp block other than the headers files and traces.sample_ratio need a restart of the agent")
	}
	p.sampler.set(s.Traces.SampleRatio)
	return p.conns.reload()
}

// sameExceptReloadable compares two settings without the keys a reload
// applies.
func sameExceptReloadable(a, b *Settings) bool {
	ac, bc := *a, *b
	ac.Traces.SampleRatio, bc.Traces.SampleRatio = 0, 0
	ac.Logs.Level, bc.Logs.Level = 0, 0
	ac.Warnings, bc.Warnings = nil, nil
	return reflect.DeepEqual(ac, bc)
}

// Shutdown flushes and stops the traces first, then the metrics, within the
// deadline of ctx, and closes the connections. The export errors of the
// final flush are not logged; the caller reports the returned error.
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	tracer, meter := p.tracer, p.meter
	p.mu.Unlock()
	p.errs.quiet.Store(true)

	var errs []error
	switch {
	case tracer != nil:
		errs = append(errs, tracer.Shutdown(ctx))
	case p.spans != nil:
		errs = append(errs, p.spans.Shutdown(ctx))
	}
	switch {
	case meter != nil:
		errs = append(errs, meter.Shutdown(ctx))
	case p.metrics != nil:
		errs = append(errs, p.metrics.Shutdown(ctx))
	}
	errs = append(errs, p.conns.close())
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("telemetry: flushing the OTLP export: %w", err)
	}
	return nil
}

// ratioSampler samples new root traces at a ratio that a reload can change.
type ratioSampler struct {
	current atomic.Pointer[sdktrace.Sampler]
}

var _ sdktrace.Sampler = (*ratioSampler)(nil)

func newRatioSampler(ratio float64) *ratioSampler {
	s := &ratioSampler{}
	s.set(ratio)
	return s
}

func (s *ratioSampler) set(ratio float64) {
	inner := sdktrace.TraceIDRatioBased(ratio)
	s.current.Store(&inner)
}

func (s *ratioSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	return (*s.current.Load()).ShouldSample(p)
}

func (s *ratioSampler) Description() string {
	return (*s.current.Load()).Description()
}
