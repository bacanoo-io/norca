// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"sync/atomic"

	metrics "github.com/hashicorp/go-metrics"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// droppedKey is the go-metrics key of the counter of telemetry dropped by
// the agent before or during the export, under the service prefix:
// norca.agent.otlp.dropped, with the labels signal and reason.
var droppedKey = []string{"agent", "otlp", "dropped"}

// Reasons of a drop.
const (
	// dropQueueFull: the export queue was full (traces.max_queue_size).
	dropQueueFull = "queue_full"

	// dropExportFailed: the export of a batch failed after its retries.
	dropExportFailed = "export_failed"
)

var (
	droppedSpansQueueFull = []metrics.Label{
		{Name: "signal", Value: SignalTraces}, {Name: "reason", Value: dropQueueFull}}
	droppedSpansExportFailed = []metrics.Label{
		{Name: "signal", Value: SignalTraces}, {Name: "reason", Value: dropExportFailed}}
)

// spanQueue bounds the spans the agent holds for the export and counts the
// sampled spans it drops. The SDK's batch span processor drops a span when
// its queue is full without telling anyone, so the queue is bounded here
// instead: spanQueue counts every span it hands to the batch processor
// until the exporter is done with it (exported or failed), and drops a span
// that would take that count over the limit. The batch processor gets a
// queue as large as the limit, so it never drops one itself: the spans in
// its queue are a part of the ones counted here.
//
// The limit (traces.max_queue_size) thus covers the queue, the batch being
// assembled and the batch being exported, which the SDK's own limit does
// not; the difference is at most two batches.
type spanQueue struct {
	next  sdktrace.SpanProcessor
	limit int64

	// pending counts the spans handed to next and not yet done.
	pending atomic.Int64
}

var _ sdktrace.SpanProcessor = (*spanQueue)(nil)

// newSpanQueue creates the bounded queue in front of a batch span processor
// that exports to exp; exp reports back through done.
func newSpanQueue(exp *spanExporter, limit int, opts ...sdktrace.BatchSpanProcessorOption) *spanQueue {
	q := &spanQueue{limit: int64(limit)}
	exp.done = q.done
	opts = append(opts, sdktrace.WithMaxQueueSize(limit))
	q.next = sdktrace.NewBatchSpanProcessor(exp, opts...)
	return q
}

func (q *spanQueue) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	q.next.OnStart(ctx, s)
}

func (q *spanQueue) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	if q.pending.Add(1) > q.limit {
		q.pending.Add(-1)
		metrics.IncrCounterWithLabels(droppedKey, 1, droppedSpansQueueFull)
		return
	}
	q.next.OnEnd(s)
}

// done is called by the exporter once it is done with a batch of n spans;
// failed batches are lost and counted as dropped.
func (q *spanQueue) done(n int, failed bool) {
	q.pending.Add(-int64(n))
	if failed {
		metrics.IncrCounterWithLabels(droppedKey, float32(n), droppedSpansExportFailed)
	}
}

func (q *spanQueue) ForceFlush(ctx context.Context) error {
	return q.next.ForceFlush(ctx)
}

func (q *spanQueue) Shutdown(ctx context.Context) error {
	return q.next.Shutdown(ctx)
}
