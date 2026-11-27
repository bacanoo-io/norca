// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"net/rpc"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// WrapServerCodec wraps the codec of a net/rpc server so that every request
// it serves gets a server span (StartRPCServer): the span starts once the
// arguments are decoded and ends when the response is written, with an
// error status when the response carries an error. It returns c itself
// when t is nil (no otlp block).
//
// The wrapper keeps the span of the request in flight, so it relies on
// net/rpc's ServeRequest, which reads one request, calls the endpoint and
// writes the response on the calling goroutine before it returns. It must
// not be used with ServeCodec, which serves the requests of a codec
// concurrently.
func (t *Tracing) WrapServerCodec(c rpc.ServerCodec) rpc.ServerCodec {
	if t == nil {
		return c
	}
	return &tracingCodec{ServerCodec: c, tracing: t}
}

// tracingCodec is the codec wrapper of WrapServerCodec.
type tracingCodec struct {
	rpc.ServerCodec
	tracing *Tracing

	// method and span belong to the request being served.
	method string
	span   trace.Span
}

func (c *tracingCodec) ReadRequestHeader(r *rpc.Request) error {
	c.span = nil
	err := c.ServerCodec.ReadRequestHeader(r)
	c.method = r.ServiceMethod
	return err
}

func (c *tracingCodec) ReadRequestBody(body any) error {
	if err := c.ServerCodec.ReadRequestBody(body); err != nil {
		return err
	}
	// net/rpc reads the body into nil to discard it when the method is
	// unknown: no span for it.
	if body != nil {
		c.span = c.tracing.StartRPCServer(c.method, body)
	}
	return nil
}

func (c *tracingCodec) WriteResponse(r *rpc.Response, body any) error {
	span := c.span
	c.span = nil
	err := c.ServerCodec.WriteResponse(r, body)
	if span != nil {
		if r.Error != "" {
			span.SetStatus(codes.Error, r.Error)
		}
		span.End()
	}
	return err
}
