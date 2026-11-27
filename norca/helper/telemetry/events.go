// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bacanoo-io/norca/norca/version"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// The attributes of the event records.
const (
	AttrEventTopic     = "norca.event.topic"
	AttrEventType      = "norca.event.type"
	AttrEventKey       = "norca.event.key"
	AttrEventIndex     = "norca.event.index"
	AttrEventTruncated = "norca.event.truncated"
)

// EventsScope is the instrumentation scope of the event records, which
// tells them apart from the agent's log lines.
const EventsScope = InstrumentationName + "/events"

// MaxEventBody bounds the body of one event record: a longer payload is
// cut and the record gets norca.event.truncated = true. With
// MaxEventBatchBytes it keeps an export request below the 4 MiB a gRPC
// receiver accepts by default.
const MaxEventBody = 1 << 20

// MaxEventBatchBytes bounds the payloads of the events of one Export call;
// the caller splits larger batches.
const MaxEventBatchBytes = 2 << 20

// errNotRunning is returned by an export before the providers started or
// after they stopped.
var errNotRunning = errors.New("telemetry: the OTLP providers are not running")

// Event is an event of the event stream to export as a log record.
type Event struct {
	Topic     string
	Type      string
	Key       string
	Namespace string
	Index     uint64

	// Time is when the event was stored in the stream.
	Time time.Time

	// Payload is the JSON of the event's payload, the record's body.
	Payload []byte
}

// EventExporter sends events of the event stream as OTLP log records
// (decision 17 of the plan), synchronously: Export returns once the
// receiver accepted the records or the export failed, so the caller
// acknowledges the events only when they are out and holds back while the
// receiver is down. It keeps nothing in memory between calls.
type EventExporter struct {
	topics []string
	exp    sdklog.Exporter

	// resource tells when the agent's identity is complete: an export
	// waits for it, as the providers start before the agent and a new
	// leader may export before the agent runs.
	resource *agentResource

	// logger is bound when the providers start and cleared when they stop.
	logger atomic.Pointer[otelLoggerRef]
}

func newEventExporter(exp sdklog.Exporter, topics []string, res *agentResource) *EventExporter {
	return &EventExporter{exp: exp, topics: slices.Clone(topics), resource: res}
}

// Topics returns the exported topics.
func (e *EventExporter) Topics() []string {
	return slices.Clone(e.topics)
}

// bind makes the exporter build its records with the loggers of lp, whose
// processor is the collector of the records.
func (e *EventExporter) bind(lp *sdklog.LoggerProvider) {
	e.logger.Store(&otelLoggerRef{lp.Logger(EventsScope,
		otellog.WithInstrumentationVersion(version.GetVersion().VersionNumber()))})
}

// close stops the exports: the providers shut down.
func (e *EventExporter) close() {
	e.logger.Store(nil)
}

// Export sends the events as one batch of log records: the payload as the
// body, the topic, type, key, namespace and index as attributes. Before the
// agent's identity is complete (Identify) it waits for it, until ctx ends.
func (e *EventExporter) Export(ctx context.Context, events []Event) error {
	if e.logger.Load() == nil {
		return errNotRunning
	}
	if _, err := e.resource.wait(ctx); err != nil {
		return err
	}
	// the providers may have stopped meanwhile
	ref := e.logger.Load()
	if ref == nil {
		return errNotRunning
	}
	if len(events) == 0 {
		return nil
	}
	batch := make([]sdklog.Record, 0, len(events))
	collect := context.WithValue(ctx, collectedRecordsKey{}, &batch)
	observed := time.Now()
	for i := range events {
		ref.Emit(collect, eventRecord(&events[i], observed))
	}
	return e.exp.Export(ctx, batch)
}

// eventRecord converts an event to a log record.
func eventRecord(ev *Event, observed time.Time) otellog.Record {
	var r otellog.Record
	r.SetTimestamp(ev.Time)
	r.SetObservedTimestamp(observed)
	r.SetSeverity(otellog.SeverityInfo)
	r.SetSeverityText("INFO")
	body, truncated := cutUTF8(ev.Payload, MaxEventBody)
	r.SetBody(otellog.StringValue(body))
	r.AddAttributes(
		otellog.String(AttrEventTopic, ev.Topic),
		otellog.String(AttrEventType, ev.Type),
		otellog.String(AttrEventKey, ev.Key),
		otellog.Int64(AttrEventIndex, int64(min(ev.Index, uint64(1<<63-1)))),
	)
	if ev.Namespace != "" {
		r.AddAttributes(otellog.String(string(AttrNamespace), ev.Namespace))
	}
	if truncated {
		r.AddAttributes(otellog.Bool(AttrEventTruncated, true))
	}
	return r
}

// cutUTF8 returns b as a string of at most limit bytes, cut at a rune
// boundary, and whether it was cut.
func cutUTF8(b []byte, limit int) (string, bool) {
	if len(b) <= limit {
		return string(b), false
	}
	end := limit
	for end > 0 && !utf8.RuneStart(b[end]) {
		end--
	}
	return string(b[:end]), true
}

// collectedRecordsKey carries the slice the records of one Export call are
// collected into.
type collectedRecordsKey struct{}

// recordCollector is the processor of the events' logger provider: it
// hands every record back to the Export call that emitted it, through the
// context, instead of queueing it.
type recordCollector struct{}

var _ sdklog.Processor = recordCollector{}

func (recordCollector) OnEmit(ctx context.Context, r *sdklog.Record) error {
	if batch, ok := ctx.Value(collectedRecordsKey{}).(*[]sdklog.Record); ok {
		*batch = append(*batch, r.Clone())
	}
	return nil
}

func (recordCollector) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (recordCollector) Shutdown(context.Context) error                         { return nil }
func (recordCollector) ForceFlush(context.Context) error                       { return nil }
