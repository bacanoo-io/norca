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

	"github.com/bacanoo-io/norca/norca/version"
	"github.com/hashicorp/go-hclog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
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
// The lifecycle has three steps, because a bad file must fail the startup
// before the agent runs, the setup of the agent is to be traced, and the
// resource needs the IDs of the running agent: New (before the agent) reads
// the files and creates the exporters, Start (still before the agent)
// creates the providers with what is known of the agent then, and Identify
// (once the agent runs) completes the resource and lets the exports go out
// (agentResource). Shutdown works after any step.
type Providers struct {
	logger   hclog.Logger
	settings *Settings
	errs     *errorReporter
	conns    *connections
	sampler  *ratioSampler

	// resource is the resource of the exports, set by Start and completed
	// by Identify, which releases the exports held until then.
	resource *agentResource

	// tracing is the trace instrumentation handed to the server, the
	// client and the HTTP API; it exists from New and is bound in Start.
	tracing *Tracing

	spans   *spanExporter
	metrics *metricExporter

	// queue holds the spans until Identify; set by Start.
	queue *spanQueue

	// sink bridges go-metrics to the meter provider (metrics signal on);
	// it exists from New and is bound in Start.
	sink *OTelSink

	// logs exports the agent's log lines, which logSink takes from the
	// intercept logger (logs signal on); both exist from New, and the sink
	// is bound in Identify: it keeps the lines until then.
	logs    *logExporter
	logSink *LogSink

	// logQueue bounds and counts the log records; set by Start.
	logQueue *logQueue

	// events exports the event stream (logs.events on); it exists from New
	// and is bound in Start; its exports wait for Identify.
	events *EventExporter

	mu             sync.Mutex
	started        bool
	identified     bool
	stopped        bool
	tracer         *sdktrace.TracerProvider
	meter          *sdkmetric.MeterProvider
	logProvider    *sdklog.LoggerProvider
	eventsProvider *sdklog.LoggerProvider
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
		resource: newAgentResource(),
		tracing:  newTracing(s.Traces),
	}
	if err := p.newExporters(); err != nil {
		if p.spans != nil {
			_ = p.spans.Shutdown(context.Background())
		}
		if p.logs != nil {
			_ = p.logs.Shutdown(context.Background())
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
		p.spans = &spanExporter{next: exp, errs: p.errs, resource: p.resource}
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
		p.metrics = &metricExporter{Exporter: exp, errs: p.errs, resource: p.resource}
		p.sink = NewOTelSink(nil, OTelSinkOptions{Logger: p.logger})
	}
	if s.Logs.Enabled {
		c, err := p.conns.get(s.Logs.Target)
		if err != nil {
			return fmt.Errorf("telemetry.otlp.logs: %w", err)
		}
		exp, err := otlploggrpc.New(context.Background(), logOptions(s, c)...)
		if err != nil {
			return fmt.Errorf("telemetry.otlp.logs: creating the exporter: %w", err)
		}
		p.logs = &logExporter{Exporter: exp, errs: p.errs, signal: SignalLogs}
		p.logSink = NewLogSink(s.Logs.Level)
		if s.Logs.Events.Enabled {
			// an exporter of its own on the same connection: the events are
			// exported synchronously, outside the batch processor
			exp, err := otlploggrpc.New(context.Background(), logOptions(s, c)...)
			if err != nil {
				return fmt.Errorf("telemetry.otlp.logs.events: creating the exporter: %w", err)
			}
			p.events = newEventExporter(&logExporter{Exporter: exp, errs: p.errs, signal: SignalEvents},
				s.Logs.Events.Topics, p.resource)
		}
	}
	return nil
}

// Start creates the providers, installs the error handler and the global
// propagator, and starts the periodic metric export. It is called once,
// before the agent, with what is known of it then (id without the IDs of
// the client and the server): from now on the agent's work is traced and
// measured, while the exports wait for Identify.
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
	p.resource.set(res)
	p.conns.pinResource(res)
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
		t := p.settings.Traces
		p.queue = newSpanQueue(p.spans, t.MaxQueueSize,
			sdktrace.WithMaxExportBatchSize(t.MaxExportBatch),
			sdktrace.WithBatchTimeout(batchTimeout),
			sdktrace.WithExportTimeout(p.settings.Timeout),
		)
		topts = append(topts, sdktrace.WithSpanProcessor(p.queue))
	}
	p.tracer = sdktrace.NewTracerProvider(topts...)
	p.tracing.bind(p.tracer)

	if p.metrics != nil {
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

	if p.logs != nil {
		p.logQueue = newLogQueue(p.logs, logMaxQueueSize,
			sdklog.WithExportMaxBatchSize(logMaxExportBatch),
			sdklog.WithExportInterval(logExportInterval),
			sdklog.WithExportTimeout(p.settings.Timeout),
		)
		p.logProvider = sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithAttributeCountLimit(logAttributeCountMax),
			sdklog.WithAttributeValueLengthLimit(logValueLengthMax),
			sdklog.WithProcessor(p.logQueue),
		)
	}
	if p.events != nil {
		// the events cut their bodies themselves (MaxEventBody)
		p.eventsProvider = sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithAttributeCountLimit(logAttributeCountMax),
			sdklog.WithAttributeValueLengthLimit(-1),
			sdklog.WithProcessor(recordCollector{}),
		)
		p.events.bind(p.eventsProvider)
	}
	p.started = true
	return nil
}

// Identify completes the resource with the IDs of the running agent and
// lets the exports held since Start go out with it: the spans that ended
// meanwhile, a metric export waiting, the log lines of the setup, the
// events. It is called once, after Start.
func (p *Providers) Identify(id Identity) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.stopped || p.identified {
		return errors.New("telemetry: the providers are not started, are identified already or stopped")
	}
	res, err := newResource(context.Background(), id, p.settings.ResourceAttributes)
	if err != nil {
		return err
	}
	// the resource first, so whatever the release lets go carries it
	p.resource.set(res)
	p.conns.pinResource(res)
	p.release()
	p.identified = true
	return nil
}

// release lets the held exports go: the spans to the batch processor, the
// waiting metric and event exports, and the log lines the sink kept to the
// log provider. Called with p.mu held, by Identify or by a shutdown before
// it.
func (p *Providers) release() {
	p.resource.release()
	if p.queue != nil {
		p.queue.release()
	}
	if p.logSink != nil && p.logProvider != nil {
		p.logSink.Bind(p.logProvider.Logger(InstrumentationName,
			otellog.WithInstrumentationVersion(version.GetVersion().VersionNumber())))
		if n := p.logSink.EarlyDropped(); n > 0 {
			countDropped(float32(n), droppedLogsSetupQueueFull)
			p.logger.Warn("log lines of the agent's setup were not exported: the export keeps a limited number until it starts",
				"dropped", n, "kept", earlyLogRecords)
		}
	}
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

// LogSink returns the hclog sink of the logs signal, to register on the
// agent's intercept logger; nil when the logs export is off, so no sink is
// registered. It exists before Start and keeps the lines of the agent's
// setup until Identify.
func (p *Providers) LogSink() *LogSink {
	if p == nil {
		return nil
	}
	return p.logSink
}

// EventExporter returns the exporter of the event stream for the leader;
// nil when logs.events is off.
func (p *Providers) EventExporter() *EventExporter {
	if p == nil {
		return nil
	}
	return p.events
}

// ForceFlush exports what the providers hold. Before Identify the held
// spans stay held and the metrics wait for it within the deadline of ctx.
func (p *Providers) ForceFlush(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	tracer, meter, logProvider := p.tracer, p.meter, p.logProvider
	p.mu.Unlock()
	var errs []error
	if tracer != nil {
		errs = append(errs, tracer.ForceFlush(ctx))
	}
	if meter != nil {
		errs = append(errs, meter.ForceFlush(ctx))
	}
	if logProvider != nil {
		errs = append(errs, logProvider.ForceFlush(ctx))
	}
	return errors.Join(errs...)
}

// Reload applies a reloaded otlp block: the headers files are read again,
// and traces.sample_ratio and logs.level take effect. Any other change
// needs a restart, which is logged; so does enabling or disabling a signal
// or the event export. An error leaves the running settings as they were.
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
		p.logger.Warn("changes of the otlp block other than the headers files, traces.sample_ratio and logs.level need a restart of the agent")
	}
	p.sampler.set(s.Traces.SampleRatio)
	if p.logSink != nil {
		p.logSink.SetLevel(s.Logs.Level)
	}
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

// Shutdown flushes and stops the traces first, then the metrics, then the
// logs (so the lines of the shutdown go out), within the deadline of ctx,
// and closes the connections. The export errors of the final flush are not
// logged; the caller reports the returned error. A shutdown between Start
// and Identify (the agent failed to start) exports what was held, with the
// resource known at Start.
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
	if !p.identified {
		p.release()
	}
	tracer, meter, logProvider, eventsProvider := p.tracer, p.meter, p.logProvider, p.eventsProvider
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
	if p.events != nil {
		p.events.close()
		if eventsProvider != nil {
			errs = append(errs, eventsProvider.Shutdown(ctx))
		}
		errs = append(errs, p.events.exp.Shutdown(ctx))
	}
	if p.logSink != nil {
		p.logSink.close()
	}
	switch {
	case logProvider != nil:
		failures := p.logQueue.failures.Load()
		errs = append(errs, logProvider.Shutdown(ctx), p.logQueue.errSince(failures))
	case p.logs != nil:
		errs = append(errs, p.logs.Shutdown(ctx))
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
