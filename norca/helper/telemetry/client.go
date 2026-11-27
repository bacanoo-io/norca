// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

// RPCCaller is a msgpack RPC client of the servers, as the client agent's
// RPC method is.
type RPCCaller interface {
	RPC(method string, args any, reply any) error
}

// TraceRPCs wraps rpc so that a call made while phase returns a context
// with a span (an allocation or task phase on a client) gets a client span
// named after the method, a child of that span, whose traceparent the
// arguments carry: the server's span of the call is its child. Calls made
// outside a phase, and arguments that carry no trace context, go through
// as they are. It returns rpc itself when t is nil.
func (t *Tracing) TraceRPCs(rpc RPCCaller, phase func() context.Context) RPCCaller {
	if t == nil || rpc == nil {
		return rpc
	}
	return &tracedRPC{inner: rpc, tracing: t, phase: phase}
}

type tracedRPC struct {
	inner   RPCCaller
	tracing *Tracing
	phase   func() context.Context
}

func (r *tracedRPC) RPC(method string, args any, reply any) error {
	carrier, ok := args.(TraceCarrier)
	if !ok {
		return r.inner.RPC(method, args, reply)
	}
	_, span := r.tracing.StartIn(r.phase(), method, trace.SpanKindClient, r.tracing.rpcAttributes(method)...)
	if !span.SpanContext().IsValid() {
		return r.inner.RPC(method, args, reply)
	}
	carrier.SetTraceParent(FormatTraceParent(span.SpanContext()))
	err := r.inner.RPC(method, args, reply)
	EndSpan(span, err)
	return err
}

// rpcAttributes returns the rpc.* attributes of a method, shared with the
// server spans of the method.
func (t *Tracing) rpcAttributes(method string) []attribute.KeyValue {
	service, name, _ := strings.Cut(method, ".")
	return []attribute.KeyValue{rpcSystem, AttrRPCService.String(service), AttrRPCMethod.String(name)}
}

// excludedPluginMethods are the long-lived calls of the plugin clients that
// never get a span: the streams of the drivers and devices, and the wait
// for a task's exit, which lasts as long as the task.
var excludedPluginMethods = []string{
	"/WaitTask", "/TaskEvents", "/TaskStats", "/ExecTaskStreaming", "/ExecTaskStreamingRaw",
	"/Fingerprint", "/Stats",
}

// excludedPluginMethod reports whether a full gRPC method name
// (/package.Service/Method) is one of excludedPluginMethods.
func excludedPluginMethod(fullMethod string) bool {
	for _, m := range excludedPluginMethods {
		if strings.HasSuffix(fullMethod, m) {
			return true
		}
	}
	return false
}

// PluginDialOptions returns the gRPC dial options of the agent's clients of
// driver plugins: a client span per call made within a span of the agent
// (a task's start, for instance), with the agent's tracer provider. The
// device plugins do not get them: the device manager calls them with its
// own context, never within a span (Reserve takes no context), so they
// would only be filtered. Calls made outside a span and the long streams start no span;
// the plugin process side is not traced, and no trace context is sent to
// it. It returns nil when t is nil.
func (t *Tracing) PluginDialOptions() []grpc.DialOption {
	if t == nil {
		return nil
	}
	return []grpc.DialOption{grpc.WithStatsHandler(t.pluginClientHandler())}
}

// pluginClientHandler returns the otelgrpc client handler of the plugin
// clients, behind the filter of joinOnlyHandler.
func (t *Tracing) pluginClientHandler() stats.Handler {
	return joinOnlyHandler{inner: otelgrpc.NewClientHandler(
		otelgrpc.WithTracerProvider(lazyTracerProvider{t: t}),
		otelgrpc.WithMeterProvider(noop.NewMeterProvider()),
		otelgrpc.WithPropagators(propagation.NewCompositeTextMapPropagator()),
	)}
}

// joinOnlyHandler passes to the otelgrpc handler only the calls made
// within a span and not excluded: otelgrpc would start a root span for
// every other call (fingerprints, stats, waits), which would only use up
// the sample ratio. The calls it passes on are marked in their context, so
// that the handler sees the stats of those calls only.
type joinOnlyHandler struct {
	inner stats.Handler
}

type joinedRPCKey struct{}

func (h joinOnlyHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() || excludedPluginMethod(info.FullMethodName) {
		return ctx
	}
	return h.inner.TagRPC(context.WithValue(ctx, joinedRPCKey{}, true), info)
}

func (h joinOnlyHandler) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	if ctx.Value(joinedRPCKey{}) != nil {
		h.inner.HandleRPC(ctx, rs)
	}
}

func (h joinOnlyHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h joinOnlyHandler) HandleConn(context.Context, stats.ConnStats) {}
