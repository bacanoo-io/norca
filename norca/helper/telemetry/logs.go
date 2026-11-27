// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
)

// Fixed values of the log SDK that would otherwise come from OTEL_*
// variables: the defaults of the batch log record processor.
const (
	logMaxQueueSize      = 2048
	logMaxExportBatch    = 512
	logExportInterval    = time.Second
	logAttributeCountMax = 128

	// logValueLengthMax bounds a string attribute (the SDK's attribute
	// value limit) and the body of an agent log record (the log sink cuts
	// it, as the SDK applies no limit to a body), so that one huge line
	// can't make a batch too large for the receiver.
	logValueLengthMax = 16 << 10
)

func logOptions(s *Settings, c *connection) []otlploggrpc.Option {
	return []otlploggrpc.Option{
		otlploggrpc.WithGRPCConn(c.conn),
		otlploggrpc.WithHeaders(map[string]string{}),
		otlploggrpc.WithTimeout(s.Timeout),
		otlploggrpc.WithRetry(otlploggrpc.RetryConfig{
			Enabled:         s.Retry.Enabled,
			InitialInterval: s.Retry.InitialInterval,
			MaxInterval:     s.Retry.MaxInterval,
			MaxElapsedTime:  s.Retry.MaxElapsed,
		}),
	}
}

// logExporter reports the export errors of the logs and events signals.
// The log SDK hands the error of a batch to the global error handler too,
// which skips it as an exportError.
type logExporter struct {
	sdklog.Exporter
	errs   *errorReporter
	signal string
}

var _ sdklog.Exporter = (*logExporter)(nil)

func (e *logExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := e.Exporter.Export(ctx, records); err != nil {
		e.errs.export(e.signal, err)
		return &exportError{signal: e.signal, err: err}
	}
	return nil
}

var (
	droppedLogsQueueFull = []metrics.Label{
		{Name: "signal", Value: SignalLogs}, {Name: "reason", Value: dropQueueFull}}
	droppedLogsExportFailed = []metrics.Label{
		{Name: "signal", Value: SignalLogs}, {Name: "reason", Value: dropExportFailed}}
	droppedLogsSetupQueueFull = []metrics.Label{
		{Name: "signal", Value: SignalLogs}, {Name: "reason", Value: dropSetupQueueFull}}
)

// logQueue bounds the log records the agent holds for the export and counts
// the ones it drops, as spanQueue does for the spans: the batch log
// processor of the SDK drops the oldest record of a full queue and reports
// it only to the OpenTelemetry global logger, which the agent does not set
// (otel.SetLogger would be a process-wide setting, and a log line is not a
// count). logQueue counts every record it hands to the batch processor
// until the exporter is done with it (exported or failed), and drops a
// record that would take that count over the limit; the batch processor's
// queue is as large as the limit, so it never drops one itself.
type logQueue struct {
	next  sdklog.Processor
	limit int64

	// pending counts the records handed to next and not yet done.
	pending atomic.Int64

	// failures counts the failed exports, and lastErr is the error of the
	// latest one, for the shutdown (queueExporter returns no error).
	failures atomic.Int64
	lastErr  atomic.Pointer[error]
}

var _ sdklog.Processor = (*logQueue)(nil)

// newLogQueue creates the bounded queue in front of a batch log processor
// that exports to exp.
func newLogQueue(exp sdklog.Exporter, limit int, opts ...sdklog.BatchProcessorOption) *logQueue {
	q := &logQueue{limit: int64(limit)}
	opts = append(opts, sdklog.WithMaxQueueSize(limit))
	q.next = sdklog.NewBatchProcessor(queueExporter{Exporter: exp, q: q}, opts...)
	return q
}

// queueExporter is the exporter of a logQueue's batch processor: it tells
// the queue when the export of a batch is over and does not return its
// error. The SDK exports a flush of its queue in chunks and drops the
// chunks after a failed one without counting them; with no error returned
// every chunk is tried (past the export deadline they fail at once) and
// counted. The error was reported by the logExporter already, and the
// queue keeps the last one for the shutdown.
type queueExporter struct {
	sdklog.Exporter
	q *logQueue
}

func (e queueExporter) Export(ctx context.Context, records []sdklog.Record) error {
	err := e.Exporter.Export(ctx, records)
	e.q.done(len(records), err)
	return nil
}

// errSince returns the error of the latest failed export when more than
// failures exports failed so far, otherwise nil.
func (q *logQueue) errSince(failures int64) error {
	if q.failures.Load() <= failures {
		return nil
	}
	if err := q.lastErr.Load(); err != nil {
		return *err
	}
	return nil
}

func (q *logQueue) OnEmit(ctx context.Context, r *sdklog.Record) error {
	if q.pending.Add(1) > q.limit {
		q.pending.Add(-1)
		countDropped(1, droppedLogsQueueFull)
		return nil
	}
	return q.next.OnEmit(ctx, r)
}

// done is called by the exporter once it is done with a batch of n
// records; failed batches are lost and counted as dropped.
func (q *logQueue) done(n int, err error) {
	q.pending.Add(-int64(n))
	if err != nil {
		q.lastErr.Store(&err)
		q.failures.Add(1)
		countDropped(float32(n), droppedLogsExportFailed)
	}
}

func (q *logQueue) Enabled(ctx context.Context, p sdklog.EnabledParameters) bool {
	return q.next.Enabled(ctx, p)
}

func (q *logQueue) ForceFlush(ctx context.Context) error {
	return q.next.ForceFlush(ctx)
}

func (q *logQueue) Shutdown(ctx context.Context) error {
	return q.next.Shutdown(ctx)
}

// Shutdown stops the exporter within the deadline of ctx. The OTLP log
// exporter's Shutdown waits for an export in flight, which with the
// receiver down lasts up to the export timeout; past the deadline the wait
// goes on in the background, until the closing of the connection (the next
// step of the shutdown) ends the export.
func (e *logExporter) Shutdown(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- e.Exporter.Shutdown(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// logsExportMethod is the gRPC method of the OTLP logs service.
const logsExportMethod = "/opentelemetry.proto.collector.logs.v1.LogsService/Export"

// resourcePin replaces the resource of every OTLP logs export request on a
// connection with the agent's. The log SDK merges the attributes of
// OTEL_RESOURCE_ATTRIBUTES into the resource it is given, and its records
// have no way to replace it (the traces and metrics exporters replace it on
// the data instead), so the request is corrected on its way out.
type resourcePin struct {
	res atomic.Pointer[pinnedResource]
}

type pinnedResource struct {
	proto     *resourcepb.Resource
	schemaURL string
}

// set pins res from now on.
func (p *resourcePin) set(res *resource.Resource) {
	p.res.Store(&pinnedResource{proto: protoResource(res), schemaURL: res.SchemaURL()})
}

// intercept is a unary client interceptor of the connection.
func (p *resourcePin) intercept(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
) error {
	if method == logsExportMethod {
		if pinned := p.res.Load(); pinned != nil {
			if lr, ok := req.(*collogspb.ExportLogsServiceRequest); ok {
				for _, rl := range lr.GetResourceLogs() {
					// the pinned message is only read (marshalled), so the
					// requests share it
					rl.Resource = pinned.proto
					rl.SchemaUrl = pinned.schemaURL
				}
			}
		}
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// protoResource converts a resource to its OTLP message.
func protoResource(res *resource.Resource) *resourcepb.Resource {
	attrs := res.Attributes()
	out := &resourcepb.Resource{Attributes: make([]*commonpb.KeyValue, 0, len(attrs))}
	for _, kv := range attrs {
		out.Attributes = append(out.Attributes, &commonpb.KeyValue{Key: string(kv.Key), Value: protoValue(kv.Value)})
	}
	return out
}

func protoValue(v attribute.Value) *commonpb.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v.AsBool()}}
	case attribute.INT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v.AsInt64()}}
	case attribute.FLOAT64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v.AsFloat64()}}
	case attribute.BOOLSLICE:
		return protoArray(v.AsBoolSlice(), func(b bool) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
		})
	case attribute.INT64SLICE:
		return protoArray(v.AsInt64Slice(), func(i int64) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}
		})
	case attribute.FLOAT64SLICE:
		return protoArray(v.AsFloat64Slice(), func(f float64) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
		})
	case attribute.STRINGSLICE:
		return protoArray(v.AsStringSlice(), func(s string) *commonpb.AnyValue {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
		})
	default:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v.String()}}
	}
}

func protoArray[T any](values []T, conv func(T) *commonpb.AnyValue) *commonpb.AnyValue {
	arr := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(values))}
	for i, v := range values {
		arr.Values[i] = conv(v)
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: arr}}
}
