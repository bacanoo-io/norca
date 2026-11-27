// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// BenchmarkWrapServerCodec serves one in-memory RPC per iteration: without
// tracing (no otlp block: no wrapper), with tracing and an unsampled root
// (the ratio's common case), and sampled into a discarding exporter. The
// difference to "off" is the cost per RPC.
func BenchmarkWrapServerCodec(b *testing.B) {
	srv := newTestRPCServer(b)

	discard := func(ratio float64) *Tracing {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSampler(NewSampler(sdktrace.TraceIDRatioBased(ratio))),
			sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(tracetest.NewNoopExporter())),
		)
		b.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		return NewTracing(tp, TracesSettings{})
	}

	cases := []struct {
		name    string
		tracing *Tracing
		parent  string
	}{
		{name: "off", tracing: nil},
		{name: "on-unsampled", tracing: discard(0)},
		{name: "on-sampled", tracing: discard(1)},
		{name: "on-sampled-parent", tracing: discard(0), parent: testTraceParent},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			c := &memCodec{method: "Test.Call", reply: &RPCTestReply{}}
			codec := tc.tracing.WrapServerCodec(c)
			b.ReportAllocs()
			for b.Loop() {
				c.args = &RPCTestArgs{TraceParent: tc.parent}
				if err := srv.ServeRequest(codec); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
