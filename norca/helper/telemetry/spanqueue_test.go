// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"github.com/shoenig/test/must"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// gateExporter holds every export until released, and fails it on demand.
type gateExporter struct {
	release chan struct{}
	fail    bool

	mu       sync.Mutex
	exported int
}

func (e *gateExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	select {
	case <-e.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if e.fail {
		return errors.New("collector down")
	}
	e.mu.Lock()
	e.exported += len(spans)
	e.mu.Unlock()
	return nil
}

func (*gateExporter) Shutdown(context.Context) error { return nil }

// droppedSpans returns the dropped-span counters of an in-memory sink.
func droppedSpans(sink *metrics.InmemSink) map[string]float64 {
	out := map[string]float64{}
	for _, interval := range sink.Data() {
		for _, c := range interval.Counters {
			if c.Name != "norca.agent.otlp.dropped" {
				continue
			}
			for _, l := range c.Labels {
				if l.Name == "reason" {
					out[l.Value] += c.Sum
				}
			}
		}
	}
	return out
}

// Not parallel: it replaces the global go-metrics sink.
func TestSpanQueue_Dropped(t *testing.T) {
	sink := metrics.NewInmemSink(time.Hour, time.Hour)
	conf := metrics.DefaultConfig(MetricsServiceName)
	conf.EnableHostname, conf.EnableRuntimeMetrics, conf.EnableHostnameLabel = false, false, false
	_, err := metrics.NewGlobal(conf, sink)
	must.NoError(t, err)
	t.Cleanup(func() { _, _ = metrics.NewGlobal(metrics.DefaultConfig(""), &metrics.BlackholeSink{}) })

	gate := &gateExporter{release: make(chan struct{})}
	exp := &spanExporter{next: gate, errs: newErrorReporter(testLogger(&syncBuffer{}))}
	queue := newSpanQueue(exp, 4,
		sdktrace.WithMaxExportBatchSize(2),
		sdktrace.WithBatchTimeout(time.Millisecond),
	)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(queue))
	tracer := tp.Tracer("test")

	// the exporter holds the first batch: 4 spans fit, the other 6 drop
	for range 10 {
		_, span := tracer.Start(t.Context(), "s")
		span.End()
	}
	must.Eq(t, map[string]float64{dropQueueFull: 6}, droppedSpans(sink))

	close(gate.release)
	must.NoError(t, tp.ForceFlush(t.Context()))
	gate.mu.Lock()
	must.Eq(t, 4, gate.exported)
	gate.mu.Unlock()
	must.Eq(t, int64(0), queue.pending.Load())

	// a failed export counts its spans as dropped
	gate.fail = true
	for range 3 {
		_, span := tracer.Start(t.Context(), "s")
		span.End()
	}
	_ = tp.ForceFlush(t.Context()) // the export error
	must.Eq(t, map[string]float64{dropQueueFull: 6, dropExportFailed: 3}, droppedSpans(sink))
	must.Eq(t, int64(0), queue.pending.Load())
	must.NoError(t, tp.Shutdown(t.Context()))
}
