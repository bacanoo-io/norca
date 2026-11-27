// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTPHandler wraps the HTTP API mux of the agent with a server span per
// request (otelhttp). It returns mux itself when t is nil (no otlp block).
//
//   - The span is named by the method and the registered mux pattern
//     ("PUT /v1/job/"), never by the raw path, and records the route, not
//     the query string.
//   - Excluded routes (health, metrics, streams, the UI) and blocking
//     queries (index > 0) get no span at all.
//   - An incoming W3C traceparent becomes a span link of a new root, unless
//     trust_http_parent is set; then it is the parent. Only the trace
//     context header is read (no baggage).
//   - The routes of the important operations are always sampled.
//   - The response carries the trace ID in the Norca-Trace-Id header.
func (t *Tracing) HTTPHandler(mux *http.ServeMux) http.Handler {
	if t == nil {
		return mux
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			w.Header().Set(TraceIDHeader, sc.TraceID().String())
		}
		mux.ServeHTTP(w, r)
	})
	trust := t.trustHTTPParent
	traced := otelhttp.NewHandler(inner, "",
		otelhttp.WithTracerProvider(lazyTracerProvider{t: t}),
		otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelhttp.WithPropagators(propagation.TraceContext{}),
		otelhttp.WithSpanNameFormatter(httpSpanName),
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return !trust }),
	)
	return &httpTracer{tracing: t, mux: mux, traced: traced}
}

// httpSpanName names an HTTP span by the method and the mux pattern.
func httpSpanName(_ string, r *http.Request) string {
	if r.Pattern == "" {
		return r.Method
	}
	return r.Method + " " + r.Pattern
}

// httpTracer routes a request around the span when it is not traced, and
// otherwise gives otelhttp the request with its mux pattern and, for the
// important routes, the always-sample mark.
type httpTracer struct {
	tracing *Tracing
	mux     *http.ServeMux
	traced  http.Handler
}

func (h *httpTracer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.tracing.Tracer() == nil {
		h.mux.ServeHTTP(w, r)
		return
	}
	_, pattern := h.mux.Handler(r)
	if excludeHTTP(r, pattern) {
		h.mux.ServeHTTP(w, r)
		return
	}
	ctx := r.Context()
	if alwaysSampleHTTP(r.Method, pattern, r.URL.Path) {
		ctx = withAlwaysSample(ctx)
	}
	// A shallow copy, so the caller's request stays as it was; the pattern
	// names the span before the mux routes the request (which sets the
	// same pattern again).
	r = r.WithContext(ctx)
	r.Pattern = pattern
	h.traced.ServeHTTP(w, r)
}
