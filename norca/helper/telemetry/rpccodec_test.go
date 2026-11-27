// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"errors"
	"net/rpc"
	"reflect"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// spanAttrs returns span attributes by key, their values as text.
func spanAttrs(kvs []attribute.KeyValue) map[attribute.Key]string {
	out := make(map[attribute.Key]string, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.Value.Emit()
	}
	return out
}

// RPCTestArgs are RPC arguments with a traceparent, as the ones embedding
// structs.InternalRpcInfo; MinQueryIndex makes it a blocking query.
type RPCTestArgs struct {
	TraceParent   string
	MinQueryIndex uint64
	Fail          bool
}

func (a *RPCTestArgs) GetTraceParent() string   { return a.TraceParent }
func (a *RPCTestArgs) SetTraceParent(tp string) { a.TraceParent = tp }
func (a *RPCTestArgs) TimeToBlock() time.Duration {
	if a.MinQueryIndex == 0 {
		return 0
	}
	return time.Minute
}

// RPCTestReply returns what the endpoint saw.
type RPCTestReply struct {
	TraceParent string
}

// Test is the RPC service of the tests.
type Test struct{}

func (Test) Call(args *RPCTestArgs, reply *RPCTestReply) error {
	reply.TraceParent = args.TraceParent
	if args.Fail {
		return errors.New("the call failed")
	}
	return nil
}

func (Test) Ping(args struct{}, reply *RPCTestReply) error { return nil }

// memCodec serves one request in memory, as helper/codec.InmemCodec does.
type memCodec struct {
	method string
	args   any
	reply  any
	err    string
}

func (c *memCodec) ReadRequestHeader(r *rpc.Request) error {
	r.ServiceMethod = c.method
	return nil
}

func (c *memCodec) ReadRequestBody(body any) error {
	if body == nil {
		return nil
	}
	reflect.ValueOf(body).Elem().Set(reflect.ValueOf(c.args).Elem())
	return nil
}

func (c *memCodec) WriteResponse(r *rpc.Response, body any) error {
	c.err = r.Error
	if c.reply != nil && r.Error == "" {
		reflect.ValueOf(c.reply).Elem().Set(reflect.ValueOf(body).Elem())
	}
	return nil
}

func (*memCodec) Close() error { return nil }

func newTestRPCServer(t testing.TB) *rpc.Server {
	t.Helper()
	srv := rpc.NewServer()
	must.NoError(t, srv.Register(Test{}))
	return srv
}

// serve serves one call through the codec wrapper of tr.
func serve(t testing.TB, srv *rpc.Server, tr *Tracing, method string, args any) *memCodec {
	t.Helper()
	c := &memCodec{method: method, args: args, reply: &RPCTestReply{}}
	must.NoError(t, srv.ServeRequest(tr.WrapServerCodec(c)))
	return c
}

func TestWrapServerCodec_Nil(t *testing.T) {
	ci.Parallel(t)

	var tr *Tracing
	c := &memCodec{}
	must.True(t, tr.WrapServerCodec(c) == rpc.ServerCodec(c))
}

func TestWrapServerCodec_ServerSpan(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 0, TracesSettings{})
	srv := newTestRPCServer(t)

	args := &RPCTestArgs{TraceParent: testTraceParent}
	c := serve(t, srv, tr, "Test.Call", args)
	must.Eq(t, "", c.err)
	must.Eq(t, testTraceParent, args.TraceParent, must.Sprint("the caller's arguments are not changed"))

	spans := exp.GetSpans()
	must.Len(t, 1, spans)
	span := spans[0]
	must.Eq(t, "Test.Call", span.Name)
	must.Eq(t, trace.SpanKindServer, span.SpanKind)
	must.Eq(t, testTraceID, span.SpanContext.TraceID())
	must.Eq(t, testSpanID, span.Parent.SpanID())
	must.True(t, span.Parent.IsRemote())
	attrs := spanAttrs(span.Attributes)
	must.Eq(t, "norca", attrs["rpc.system"])
	must.Eq(t, "Test", attrs["rpc.service"])
	must.Eq(t, "Call", attrs["rpc.method"])
	must.Eq(t, codes.Unset, span.Status.Code)

	// the endpoint saw the server span as the parent of what it does
	reply := c.reply.(*RPCTestReply)
	must.Eq(t, FormatTraceParent(span.SpanContext), reply.TraceParent)
}

func TestWrapServerCodec_ErrorStatus(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 0, TracesSettings{})
	srv := newTestRPCServer(t)

	c := serve(t, srv, tr, "Test.Call", &RPCTestArgs{TraceParent: testTraceParent, Fail: true})
	must.Eq(t, "the call failed", c.err)
	spans := exp.GetSpans()
	must.Len(t, 1, spans)
	must.Eq(t, codes.Error, spans[0].Status.Code)
	must.Eq(t, "the call failed", spans[0].Status.Description)
}

func TestWrapServerCodec_Roots(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 0, TracesSettings{})
	srv := newTestRPCServer(t)

	// a root at ratio 0 is not sampled, but has a trace ID for the logs
	c := serve(t, srv, tr, "Test.Call", &RPCTestArgs{})
	must.SliceEmpty(t, exp.GetSpans())
	sc, ok := ParseTraceParent(c.reply.(*RPCTestReply).TraceParent)
	must.True(t, ok)
	must.False(t, sc.IsSampled())

	// an argument without a traceparent field gets a root span as well
	serve(t, srv, tr, "Test.Ping", &struct{}{})
	must.SliceEmpty(t, exp.GetSpans())
}

func TestWrapServerCodec_NoSpan(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 1, TracesSettings{ExcludeRPC: []string{"Test.Call"}})
	srv := newTestRPCServer(t)

	// excluded method
	c := serve(t, srv, tr, "Test.Call", &RPCTestArgs{TraceParent: testTraceParent})
	must.Eq(t, testTraceParent, c.reply.(*RPCTestReply).TraceParent)

	// blocking query
	tr, exp2 := newTestTracing(t, 1, TracesSettings{})
	c = serve(t, srv, tr, "Test.Call", &RPCTestArgs{TraceParent: testTraceParent, MinQueryIndex: 5})
	must.Eq(t, testTraceParent, c.reply.(*RPCTestReply).TraceParent)

	// unknown method: the body is discarded, an error answered, no span
	c = &memCodec{method: "Test.Unknown", args: &RPCTestArgs{}}
	must.Error(t, srv.ServeRequest(tr.WrapServerCodec(c)))
	must.StrContains(t, c.err, "can't find method")

	must.SliceEmpty(t, exp.GetSpans())
	must.SliceEmpty(t, exp2.GetSpans())
}

// The wrapper keeps the span of the request in flight, which relies on
// ServeRequest answering before it returns: consecutive requests on one
// codec get one span each.
func TestWrapServerCodec_Sequential(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 1, TracesSettings{})
	srv := newTestRPCServer(t)
	c := &memCodec{reply: &RPCTestReply{}}
	codec := tr.WrapServerCodec(c)
	for range 3 {
		c.method, c.args = "Test.Call", &RPCTestArgs{}
		must.NoError(t, srv.ServeRequest(codec))
	}
	spans := exp.GetSpans()
	must.Len(t, 3, spans)
	must.NotEq(t, spans[0].SpanContext.TraceID(), spans[1].SpanContext.TraceID())
}
