// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package drivers

import (
	"context"
	"net"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/shoenig/test/must"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// startDriver is a driver whose tasks start and exit at once.
type startDriver struct {
	DriverPlugin
}

func (startDriver) StartTask(cfg *TaskConfig) (*TaskHandle, *DriverNetwork, error) {
	return &TaskHandle{Version: 1, Config: cfg, State: TaskStateRunning}, nil, nil
}

func (startDriver) WaitTask(context.Context, string) (<-chan *ExitResult, error) {
	ch := make(chan *ExitResult, 1)
	ch <- &ExitResult{}
	return ch, nil
}

// testTracedDriverClient serves d over gRPC and returns its client, dialed
// with the plugin dial options of tracing.
func testTracedDriverClient(t *testing.T, d DriverPlugin, tracing *telemetry.Tracing) *driverPluginClient {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	server := grpc.NewServer()
	must.NoError(t, NewDriverPlugin(d, testlog.HCLogger(t)).GRPCServer(nil, server))
	go func() { _ = server.Serve(l) }()
	t.Cleanup(server.Stop)

	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		tracing.PluginDialOptions()...)
	conn, err := grpc.NewClient(l.Addr().String(), opts...)
	must.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	raw, err := NewDriverPlugin(nil, testlog.HCLogger(t)).GRPCClient(context.Background(), nil, conn)
	must.NoError(t, err)
	return raw.(*driverPluginClient)
}

// A driver plugin call made within a span is a client span below it; calls
// outside a span and the long waits start no span.
func TestDriverPluginClient_Tracing(t *testing.T) {
	ci.Parallel(t)

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracing := telemetry.NewTracing(tp, telemetry.TracesSettings{})

	client := testTracedDriverClient(t, startDriver{}, tracing)
	var _ TaskStarterContext = client

	// outside a span: no span
	_, _, err := client.StartTask(&TaskConfig{ID: "a"})
	must.NoError(t, err)
	must.SliceEmpty(t, exp.GetSpans())

	ctx, parent := tracing.Tracer().Start(context.Background(), telemetry.SpanTaskStart)
	_, _, err = client.StartTaskContext(ctx, &TaskConfig{ID: "b"})
	must.NoError(t, err)
	ch, err := client.WaitTask(ctx, "b")
	must.NoError(t, err)
	<-ch
	parent.End()

	spans := exp.GetSpans()
	must.Len(t, 2, spans, must.Sprint("StartTask and the parent, no WaitTask"))
	call := spans[0]
	must.Eq(t, "hashicorp.nomad.plugins.drivers.proto.Driver/StartTask", call.Name)
	must.Eq(t, trace.SpanKindClient, call.SpanKind)
	must.Eq(t, parent.SpanContext().SpanID(), call.Parent.SpanID())
}

// Without tracing the plugin clients get no dial options.
func TestDriverPluginClient_NoTracing(t *testing.T) {
	ci.Parallel(t)
	var tracing *telemetry.Tracing
	must.Nil(t, tracing.PluginDialOptions())
}
