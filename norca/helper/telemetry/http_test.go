// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/trace"
)

// testMux is a mux with the shapes of the agent's routes; every handler
// answers with the traceparent its request context carries.
func testMux() *http.ServeMux {
	mux := http.NewServeMux()
	answer := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(TraceParentFromContext(r.Context())))
	}
	mux.HandleFunc("/v1/jobs", answer)
	mux.HandleFunc("/v1/job/", answer)
	mux.HandleFunc("/v1/agent/health", answer)
	mux.HandleFunc("/v1/client/fs/", answer)
	mux.HandleFunc("/v1/fail", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	return mux
}

func request(h http.Handler, method, url string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, nil)
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHTTPHandler_Nil(t *testing.T) {
	ci.Parallel(t)

	var tr *Tracing
	mux := testMux()
	must.True(t, tr.HTTPHandler(mux) == http.Handler(mux))

	// not bound yet: requests pass without a span
	h := newTracing(TracesSettings{}).HTTPHandler(mux)
	w := request(h, "PUT", "/v1/jobs", nil)
	must.Eq(t, "", w.Body.String())
	must.Eq(t, "", w.Header().Get(TraceIDHeader))
}

func TestHTTPHandler_Span(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 1, TracesSettings{})
	h := tr.HTTPHandler(testMux())

	w := request(h, "GET", "/v1/job/web/allocations?namespace=prod&token=secret", nil)
	must.Eq(t, http.StatusOK, w.Code)

	spans := exp.GetSpans()
	must.Len(t, 1, spans)
	span := spans[0]
	must.Eq(t, "GET /v1/job/", span.Name, must.Sprint("named by the route, never the raw path"))
	must.Eq(t, trace.SpanKindServer, span.SpanKind)
	must.Eq(t, span.SpanContext.TraceID().String(), w.Header().Get(TraceIDHeader))

	attrs := spanAttrs(span.Attributes)
	must.Eq(t, "/v1/job/", attrs["http.route"])
	must.Eq(t, "/v1/job/web/allocations", attrs["url.path"])
	for k, v := range attrs {
		must.StrNotContains(t, v, "secret", must.Sprintf("the query string is not recorded (%s)", k))
	}

	// the handler sees the span, and the RPC arguments get its traceparent
	must.Eq(t, FormatTraceParent(span.SpanContext), w.Body.String())

	w = request(h, "GET", "/v1/fail", nil)
	must.Eq(t, http.StatusInternalServerError, w.Code)
	must.Len(t, 2, exp.GetSpans())
}

func TestHTTPHandler_Excluded(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 1, TracesSettings{})
	h := tr.HTTPHandler(testMux())

	for _, url := range []string{
		"/v1/agent/health",
		"/v1/jobs?index=42",
		"/v1/client/fs/logs/abc",
	} {
		w := request(h, "GET", url, nil)
		must.Eq(t, "", w.Body.String(), must.Sprint(url))
		must.Eq(t, "", w.Header().Get(TraceIDHeader), must.Sprint(url))
	}
	must.SliceEmpty(t, exp.GetSpans())
}

func TestHTTPHandler_AlwaysSampled(t *testing.T) {
	ci.Parallel(t)

	tr, exp := newTestTracing(t, 0, TracesSettings{})
	h := tr.HTTPHandler(testMux())

	request(h, "GET", "/v1/jobs", nil)
	must.SliceEmpty(t, exp.GetSpans(), must.Sprint("a read at ratio 0"))

	w := request(h, "PUT", "/v1/jobs", nil)
	spans := exp.GetSpans()
	must.Len(t, 1, spans)
	must.Eq(t, "PUT /v1/jobs", spans[0].Name)
	sc, ok := ParseTraceParent(w.Body.String())
	must.True(t, ok)
	must.True(t, sc.IsSampled(), must.Sprint("the RPC it calls follows the decision"))
}

func TestHTTPHandler_IncomingTraceParent(t *testing.T) {
	ci.Parallel(t)

	incoming := http.Header{"Traceparent": []string{testTraceParent}}

	t.Run("untrusted is a link", func(t *testing.T) {
		tr, exp := newTestTracing(t, 0, TracesSettings{})
		h := tr.HTTPHandler(testMux())

		// a sampled incoming context does not force sampling
		request(h, "GET", "/v1/jobs", incoming)
		must.SliceEmpty(t, exp.GetSpans())

		request(h, "PUT", "/v1/jobs", incoming)
		spans := exp.GetSpans()
		must.Len(t, 1, spans)
		span := spans[0]
		must.False(t, span.Parent.IsValid(), must.Sprint("a new root"))
		must.NotEq(t, testTraceID, span.SpanContext.TraceID())
		must.Len(t, 1, span.Links)
		must.Eq(t, testTraceID, span.Links[0].SpanContext.TraceID())
		must.Eq(t, testSpanID, span.Links[0].SpanContext.SpanID())
	})

	t.Run("trusted is the parent", func(t *testing.T) {
		tr, exp := newTestTracing(t, 0, TracesSettings{TrustHTTPParent: true})
		h := tr.HTTPHandler(testMux())

		w := request(h, "GET", "/v1/jobs", incoming)
		spans := exp.GetSpans()
		must.Len(t, 1, spans, must.Sprint("the sampled parent decides"))
		span := spans[0]
		must.Eq(t, testTraceID, span.SpanContext.TraceID())
		must.Eq(t, testSpanID, span.Parent.SpanID())
		must.SliceEmpty(t, span.Links)
		must.Eq(t, testTraceID.String(), w.Header().Get(TraceIDHeader))
	})
}
