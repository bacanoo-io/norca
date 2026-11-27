// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/hashicorp/go-hclog"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// BenchmarkEvalBroker_Cycle measures an enqueue, dequeue (with the trace
// parent lookup of Eval.Dequeue) and ack of an evaluation, untraced as
// without an otlp block, and traced.
func BenchmarkEvalBroker_Cycle(b *testing.B) {
	for _, bc := range []struct {
		name   string
		traced bool
	}{{"untraced", false}, {"traced", true}} {
		b.Run(bc.name, func(b *testing.B) {
			broker, err := NewEvalBroker(time.Minute, time.Second, time.Second, 3)
			if err != nil {
				b.Fatal(err)
			}
			broker.SetEnabled(true)
			b.Cleanup(func() { broker.SetEnabled(false) })
			eval := mock.Eval()
			tp := testTraceParent(1)
			b.ReportAllocs()
			for b.Loop() {
				if bc.traced {
					broker.EnqueueTraced(eval, tp)
				} else {
					broker.Enqueue(eval)
				}
				out, token, err := broker.Dequeue(defaultSched, time.Second)
				if err != nil || out == nil {
					b.Fatal("no evaluation", err)
				}
				_ = broker.TraceParent(out.ID, token)
				if err := broker.Ack(out.ID, token); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWorker_EvalTrace measures the trace work of the worker per
// evaluation: the eval.process and scheduler spans and the traceparent of
// an in-process RPC, without an otlp block, with an unsampled parent, and
// sampled (spans dropped by a no-op processor).
func BenchmarkWorker_EvalTrace(b *testing.B) {
	unsampled := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"
	sampled := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	newTracing := func() *telemetry.Tracing {
		tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(telemetry.NewSampler(sdktrace.NeverSample())))
		b.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		return telemetry.NewTracing(tp, telemetry.TracesSettings{})
	}
	for _, bc := range []struct {
		name    string
		tracing *telemetry.Tracing
		parent  string
	}{
		{"off", nil, ""},
		{"unsampled", newTracing(), unsampled},
		{"sampled", newTracing(), sampled},
	} {
		b.Run(bc.name, func(b *testing.B) {
			w := &Worker{srv: &Server{config: &Config{Tracing: bc.tracing}}, logger: hclog.NewNullLogger()}
			eval := mock.Eval()
			b.ReportAllocs()
			for b.Loop() {
				w.startEvalTrace(eval, bc.parent)
				end := w.startSchedulerTrace(eval.Type)
				args := &mockTraceArgs{}
				w.traceRPC(args)
				end(nil)
				w.endEvalTrace(true)
			}
		})
	}
}

type mockTraceArgs struct{ tp string }

func (a *mockTraceArgs) GetTraceParent() string   { return a.tp }
func (a *mockTraceArgs) SetTraceParent(tp string) { a.tp = tp }
