// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"sync"
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

	// dropSetupQueueFull: the log sink kept earlyLogRecords lines of the
	// agent's setup already, before the export started.
	dropSetupQueueFull = "setup_queue_full"
)

// countDropped counts n records of telemetry the agent dropped; labels
// name the signal and the reason.
func countDropped(n float32, labels []metrics.Label) {
	metrics.IncrCounterWithLabels(droppedKey, n, labels)
}

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
//
// Until release (Identify, or a shutdown before it) the queue holds the
// spans itself instead of handing them on, so that none is exported before
// the agent's resource is complete (agentResource); the held spans count
// toward the limit too.
type spanQueue struct {
	next  sdktrace.SpanProcessor
	limit int64

	// pending counts the spans held or handed to next and not yet done.
	pending atomic.Int64

	// released is set once the held spans went to next; from then on
	// OnEnd hands every span on without taking mu.
	released atomic.Bool
	mu       sync.Mutex
	held     []sdktrace.ReadOnlySpan
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
	if !q.released.Load() && q.hold(s) {
		return
	}
	q.next.OnEnd(s)
}

// hold keeps s until release; it reports false when the release came first.
func (q *spanQueue) hold(s sdktrace.ReadOnlySpan) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.released.Load() {
		return false
	}
	q.held = append(q.held, s)
	return true
}

// release hands the held spans to the batch processor, in the order they
// ended, and every later span directly. Its queue is as large as the limit,
// which the held spans count toward, so it takes them all.
func (q *spanQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.released.Load() {
		return
	}
	for _, s := range q.held {
		q.next.OnEnd(s)
	}
	q.held = nil
	q.released.Store(true)
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

// Shutdown releases the held spans (a shutdown before Identify exports them
// with the resource known then) and shuts the batch processor down.
func (q *spanQueue) Shutdown(ctx context.Context) error {
	q.release()
	return q.next.Shutdown(ctx)
}
