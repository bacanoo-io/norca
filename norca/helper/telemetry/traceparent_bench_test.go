// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func BenchmarkFormatTraceParent(b *testing.B) {
	sc := testSpanContext(true)
	b.ReportAllocs()
	for b.Loop() {
		_ = FormatTraceParent(sc)
	}
}

func BenchmarkParseTraceParent(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = ParseTraceParent(testTraceParent)
	}
}

// The W3C propagator of the SDK, for comparison: what the RPC arguments
// would cost with a header carrier.
func BenchmarkTraceContextPropagator(b *testing.B) {
	prop := propagation.TraceContext{}
	ctx := trace.ContextWithSpanContext(context.Background(), testSpanContext(true))
	b.ReportAllocs()
	for b.Loop() {
		carrier := propagation.MapCarrier{}
		prop.Inject(ctx, carrier)
		_ = prop.Extract(context.Background(), carrier)
	}
}
