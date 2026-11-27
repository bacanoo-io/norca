// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var (
	testTraceID = trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	testSpanID  = trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
)

const testTraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func testSpanContext(sampled bool) trace.SpanContext {
	var flags trace.TraceFlags
	if sampled {
		flags = trace.FlagsSampled
	}
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: testTraceID, SpanID: testSpanID, TraceFlags: flags})
}

func TestFormatTraceParent(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, testTraceParent, FormatTraceParent(testSpanContext(true)))
	must.Eq(t, strings.TrimSuffix(testTraceParent, "1")+"0", FormatTraceParent(testSpanContext(false)))
	must.Eq(t, "", FormatTraceParent(trace.SpanContext{}))

	// the same as the W3C propagator writes
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(trace.ContextWithSpanContext(context.Background(), testSpanContext(true)), carrier)
	must.Eq(t, carrier.Get("traceparent"), FormatTraceParent(testSpanContext(true)))
}

func TestParseTraceParent(t *testing.T) {
	ci.Parallel(t)

	sc, ok := ParseTraceParent(testTraceParent)
	must.True(t, ok)
	must.Eq(t, testTraceID, sc.TraceID())
	must.Eq(t, testSpanID, sc.SpanID())
	must.True(t, sc.IsSampled())
	must.True(t, sc.IsRemote())

	sc, ok = ParseTraceParent(FormatTraceParent(testSpanContext(false)))
	must.True(t, ok)
	must.False(t, sc.IsSampled())

	// unknown flags are dropped, only the sampled flag is kept
	sc, ok = ParseTraceParent(strings.TrimSuffix(testTraceParent, "01") + "ff")
	must.True(t, ok)
	must.Eq(t, trace.FlagsSampled, sc.TraceFlags())

	bad := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "short", in: testTraceParent[:54]},
		{name: "long", in: testTraceParent + "-00"},
		{name: "other version", in: "01" + testTraceParent[2:]},
		{name: "upper case", in: strings.ToUpper(testTraceParent)},
		{name: "not hex", in: strings.Replace(testTraceParent, "4b", "zz", 1)},
		{name: "bad flags", in: strings.TrimSuffix(testTraceParent, "01") + "0g"},
		{name: "missing dash", in: strings.Replace(testTraceParent, "-", "_", 1)},
		{name: "zero trace id", in: "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
		{name: "zero span id", in: "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := ParseTraceParent(tc.in)
			must.False(t, ok)
		})
	}
}

func TestContextWithTraceParent(t *testing.T) {
	ci.Parallel(t)

	ctx := ContextWithTraceParent(context.Background(), testTraceParent)
	sc := trace.SpanContextFromContext(ctx)
	must.True(t, sc.IsRemote())
	must.Eq(t, testTraceID, sc.TraceID())
	must.Eq(t, testTraceParent, TraceParentFromContext(ctx))

	base := context.Background()
	must.Eq(t, base, ContextWithTraceParent(base, ""))
	must.Eq(t, base, ContextWithTraceParent(base, "garbage"))
	must.Eq(t, "", TraceParentFromContext(base))
}

func FuzzParseTraceParent(f *testing.F) {
	f.Add(testTraceParent)
	f.Add("00-00000000000000000000000000000000-0000000000000000-00")
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		sc, ok := ParseTraceParent(in)
		if !ok {
			return
		}
		// what parses formats back to the same text, up to the flags
		must.Eq(t, in[:53], FormatTraceParent(sc)[:53])
	})
}
