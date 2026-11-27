// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// carrierArgs is an RPC argument that carries a traceparent.
type carrierArgs struct{ tp string }

func (a *carrierArgs) GetTraceParent() string   { return a.tp }
func (a *carrierArgs) SetTraceParent(tp string) { a.tp = tp }

// recordingRPC records the traceparent of each call's arguments.
type recordingRPC struct {
	seen []string
	err  error
}

func (r *recordingRPC) RPC(_ string, args any, _ any) error {
	if c, ok := args.(TraceCarrier); ok {
		r.seen = append(r.seen, c.GetTraceParent())
	}
	return r.err
}

// An RPC made within a phase is a client span below the phase, whose
// traceparent the arguments carry; outside a phase the call is unchanged.
func TestTracing_TraceRPCs(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracing := NewTracing(tp, TracesSettings{})

	var phase SpanTracker
	inner := &recordingRPC{}
	rpc := tracing.TraceRPCs(inner, phase.Context)

	must.NoError(t, rpc.RPC("CSIVolume.Claim", &carrierArgs{}, nil))
	must.Eq(t, []string{""}, inner.seen)
	must.SliceEmpty(t, exp.GetSpans())

	ctx, span := tracing.Start(context.Background(), SpanAllocStart, trace.SpanKindInternal)
	phase.Set(ctx, span)
	inner.err = errors.New("denied")
	must.Error(t, rpc.RPC("CSIVolume.Claim", &carrierArgs{}, nil))
	span.End()

	spans := exp.GetSpans()
	must.Len(t, 2, spans)
	call := spans[0]
	must.Eq(t, "CSIVolume.Claim", call.Name)
	must.Eq(t, trace.SpanKindClient, call.SpanKind)
	must.Eq(t, span.SpanContext().SpanID(), call.Parent.SpanID())
	must.Eq(t, codes.Error, call.Status.Code)
	must.Eq(t, FormatTraceParent(call.SpanContext), inner.seen[1])

	// the phase ended: no span
	must.NoError(t, func() error { inner.err = nil; return rpc.RPC("CSIVolume.Claim", &carrierArgs{}, nil) }())
	must.Len(t, 2, exp.GetSpans())

	// without tracing the client is unwrapped
	var none *Tracing
	must.Eq(t, RPCCaller(inner), none.TraceRPCs(inner, phase.Context))
}

// The plugin methods of long streams and waits are excluded.
func TestExcludedPluginMethod(t *testing.T) {
	for method, excluded := range map[string]bool{
		"/hashicorp.nomad.plugins.drivers.proto.Driver/StartTask":         false,
		"/hashicorp.nomad.plugins.drivers.proto.Driver/WaitTask":          true,
		"/hashicorp.nomad.plugins.drivers.proto.Driver/TaskStats":         true,
		"/hashicorp.nomad.plugins.drivers.proto.Driver/ExecTaskStreaming": true,
		"/hashicorp.nomad.plugins.device.DevicePlugin/Stats":              true,
		"/hashicorp.nomad.plugins.device.DevicePlugin/Reserve":            false,
	} {
		must.Eq(t, excluded, excludedPluginMethod(method), must.Sprint(method))
	}
}
