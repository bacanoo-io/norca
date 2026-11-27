// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"sync/atomic"
	"time"

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
