// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// lockedBuffer is a buffer for a log sink that the agent's goroutines
// write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the logged lines that contain s.
func (b *lockedBuffer) lines(s string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(l, s) {
			out = append(out, l)
		}
	}
	return out
}

// An HTTP API request is the root of its trace: the server span of the RPC
// it makes and that span's Raft and FSM applies are its children, its
// response names the trace, and its log lines carry the trace ID. An
// incoming traceparent is a link, not the parent.
func TestHTTP_Tracing(t *testing.T) {
	ci.Parallel(t)

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(telemetry.NewSampler(sdktrace.NeverSample())),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	logger := testlog.HCLogger(t)
	logger.SetLevel(testlog.HCLoggerTestLevel())
	var logs lockedBuffer
	logger.RegisterSink(hclog.NewSinkAdapter(&hclog.LoggerOptions{
		Output: &logs, Level: hclog.Debug, JSONFormat: true,
	}))
	s := &TestAgent{
		T:          t,
		Name:       t.Name(),
		Enterprise: EnterpriseTestAgent,
		logger:     logger,
		Tracing:    telemetry.NewTracing(tp, telemetry.TracesSettings{}),
		ConfigCallback: func(c *Config) {
			c.Client.Enabled = false
		},
	}
	s.Start()
	t.Cleanup(s.Shutdown)
	testutil.WaitForLeader(t, s.Agent.RPC)

	incoming := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	body := encodeReq(api.JobRegisterRequest{Job: MockJob(), WriteRequest: api.WriteRequest{Region: "global"}})
	req, err := http.NewRequest(http.MethodPut, s.HTTPAddr()+"/v1/jobs?pretty=1", body)
	must.NoError(t, err)
	req.Header.Set("traceparent", incoming)
	resp, err := http.DefaultClient.Do(req)
	must.NoError(t, err)
	must.NoError(t, resp.Body.Close())
	must.Eq(t, http.StatusOK, resp.StatusCode)

	traceID := resp.Header.Get(telemetry.TraceIDHeader)
	must.NotEq(t, "", traceID)

	var httpSpan, rpcSpan, applySpan, fsmSpan *tracetest.SpanStub
	spans := exp.GetSpans()
	for i := range spans {
		span := &spans[i]
		if span.SpanContext.TraceID().String() != traceID {
			continue
		}
		switch span.Name {
		case "PUT /v1/jobs":
			httpSpan = span
		case "Job.Register":
			rpcSpan = span
		case "raft.apply JobRegisterRequestType":
			applySpan = span
		case "fsm.apply JobRegisterRequestType":
			fsmSpan = span
		}
	}
	must.NotNil(t, httpSpan, must.Sprint("always sampled, also at ratio 0"))
	must.NotNil(t, rpcSpan)
	must.NotNil(t, applySpan)
	must.NotNil(t, fsmSpan)

	// the incoming context is a link of a new root, not its parent
	must.False(t, httpSpan.Parent.IsValid())
	must.Len(t, 1, httpSpan.Links)
	must.Eq(t, "4bf92f3577b34da6a3ce929d0e0e4736", httpSpan.Links[0].SpanContext.TraceID().String())

	must.Eq(t, httpSpan.SpanContext.SpanID(), rpcSpan.Parent.SpanID())
	must.Eq(t, rpcSpan.SpanContext.SpanID(), applySpan.Parent.SpanID())
	must.Eq(t, applySpan.SpanContext.SpanID(), fsmSpan.Parent.SpanID())

	// the request's log line carries the trace
	complete := logs.lines(`"@message":"request complete"`)
	var matching []string
	for _, l := range complete {
		if strings.Contains(l, `"http_path":"/v1/jobs?pretty=1"`) {
			matching = append(matching, l)
		}
	}
	must.Len(t, 1, matching)
	must.StrContains(t, matching[0], `"trace_id":"`+traceID+`"`)
	must.StrContains(t, matching[0], `"span_id":"`+httpSpan.SpanContext.SpanID().String()+`"`)

	// excluded: no span, no trace ID, no trace in the log line
	before := len(exp.GetSpans())
	resp, err = http.Get(s.HTTPAddr() + "/v1/agent/health")
	must.NoError(t, err)
	must.NoError(t, resp.Body.Close())
	must.Eq(t, "", resp.Header.Get(telemetry.TraceIDHeader))
	must.Len(t, before, exp.GetSpans())
	for _, l := range logs.lines(`"http_path":"/v1/agent/health"`) {
		must.StrNotContains(t, l, "trace_id")
	}

	// a read at ratio 0 is not exported, but its trace ID is there for the
	// logs and the user
	resp, err = http.Get(s.HTTPAddr() + "/v1/jobs")
	must.NoError(t, err)
	must.NoError(t, resp.Body.Close())
	readID := resp.Header.Get(telemetry.TraceIDHeader)
	must.NotEq(t, "", readID)
	tid, err := trace.TraceIDFromHex(readID)
	must.NoError(t, err)
	must.True(t, tid.IsValid())
	must.Len(t, before, exp.GetSpans())
}

// The routes the tracing names (always sampled, excluded) are routes of the
// agent's mux, so a renamed route cannot silently fall out of the lists.
func TestHTTP_TracingRoutesRegistered(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		routes := append(telemetry.AlwaysSampledHTTPRoutes(), telemetry.ExcludedHTTPRoutes()...)
		for _, route := range routes {
			if strings.HasPrefix(route, "/debug/") || route == "/ui/" {
				continue // registered only with enable_debug, or a UI build
			}
			_, pattern := s.Server.mux.Handler(httptest.NewRequest(http.MethodPut, route, nil))
			must.Eq(t, route, pattern, must.Sprintf("route %q", route))
		}
	})
}
