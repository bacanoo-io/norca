// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// waitSpan waits for the ended span named name whose attribute key has the
// value, and returns it.
func waitSpan(t *testing.T, exp *tracetest.InMemoryExporter, name string, key, value string) tracetest.SpanStub {
	t.Helper()
	var found tracetest.SpanStub
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			for _, s := range exp.GetSpans() {
				if s.Name == name && spanAttr(s, key) == value {
					found = s
					return nil
				}
			}
			return fmt.Errorf("no span %s with %s=%s", name, key, value)
		}),
		wait.Timeout(20*time.Second),
		wait.Gap(20*time.Millisecond),
	))
	return found
}

func spanAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// childrenOf returns the names of the ended spans whose parent is span.
func childrenOf(exp *tracetest.InMemoryExporter, span tracetest.SpanStub) map[string]tracetest.SpanStub {
	out := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		if s.Parent.SpanID() == span.SpanContext.SpanID() && s.SpanContext.TraceID() == span.SpanContext.TraceID() {
			out[s.Name] = s
		}
	}
	return out
}

// The phases of an allocation on a client are traces of their own, joined
// to the scheduling trace by norca.eval.id: a job registered within a
// trace is scheduled in eval.process, and the client's alloc.start trace of
// its allocation has the same norca.eval.id, with the hooks and the task's
// start below it; its stop and its garbage collection are alloc.stop and
// alloc.gc traces.
func TestClient_TracingAllocPhases(t *testing.T) {
	ci.Parallel(t)

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(telemetry.NewSampler(sdktrace.AlwaysSample())),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracing := telemetry.NewTracing(tp, telemetry.TracesSettings{})

	s1, _, cleanupS1 := testServer(t, func(c *norca.Config) { c.Tracing = tracing })
	t.Cleanup(cleanupS1)
	testutil.WaitForLeader(t, s1.RPC)

	c1, cleanupC1 := TestClient(t, func(c *config.Config) {
		c.DevMode = false
		c.RPCHandler = s1
		c.Tracing = tracing
	})
	t.Cleanup(func() { _ = cleanupC1() })
	waitTilNodeReady(c1, t)

	job := mock.MinJob()
	_, root := tracing.Tracer().Start(context.Background(), "PUT /v1/jobs", trace.WithSpanKind(trace.SpanKindServer))
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	req.SetTraceParent(telemetry.FormatTraceParent(root.SpanContext()))
	var resp structs.JobRegisterResponse
	must.NoError(t, s1.RPC("Job.Register", req, &resp))
	root.End()

	var alloc *structs.Allocation
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			allocs, err := s1.State().AllocsByJob(nil, job.Namespace, job.ID, false)
			if err != nil || len(allocs) != 1 {
				return fmt.Errorf("expected one allocation: %v", err)
			}
			alloc = allocs[0]
			if alloc.ClientStatus != structs.AllocClientStatusRunning {
				return fmt.Errorf("allocation %s", alloc.ClientStatus)
			}
			return nil
		}),
		wait.Timeout(20*time.Second),
		wait.Gap(50*time.Millisecond),
	))

	// the scheduling trace of the registration; the allocation may have
	// been placed by a blocked evaluation it created, unblocked later in a
	// trace of its own
	registered := waitSpan(t, exp, telemetry.SpanEvalProcess, string(telemetry.AttrEvalID), resp.EvalID)
	must.Eq(t, root.SpanContext().TraceID(), registered.SpanContext.TraceID())
	process := waitSpan(t, exp, telemetry.SpanEvalProcess, string(telemetry.AttrEvalID), alloc.EvalID)

	// the client's start trace of the allocation, a root of its own
	start := waitSpan(t, exp, telemetry.SpanAllocStart, string(telemetry.AttrAllocID), alloc.ID)
	must.False(t, start.Parent.IsValid())
	must.NotEq(t, process.SpanContext.TraceID(), start.SpanContext.TraceID())
	must.Eq(t, alloc.EvalID, spanAttr(start, string(telemetry.AttrEvalID)))
	must.Eq(t, job.ID, spanAttr(start, string(telemetry.AttrJobID)))
	must.Eq(t, alloc.TaskGroup, spanAttr(start, string(telemetry.AttrTaskGroup)))

	children := childrenOf(exp, start)
	must.MapContainsKey(t, children, "alloc.hook.alloc_dir")
	taskStart, ok := children[telemetry.SpanTaskStart]
	must.True(t, ok, must.Sprintf("task.start below alloc.start: %v", children))
	task := job.TaskGroups[0].Tasks[0].Name
	must.Eq(t, task, spanAttr(taskStart, string(telemetry.AttrTask)))
	must.Eq(t, alloc.EvalID, spanAttr(taskStart, string(telemetry.AttrEvalID)))
	must.MapContainsKey(t, childrenOf(exp, taskStart), "task.hook.task_dir")

	// the stop of the job: an alloc.stop trace with the task's stop
	dereg := &structs.JobDeregisterRequest{
		JobID:        job.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	must.NoError(t, s1.RPC("Job.Deregister", dereg, &structs.JobDeregisterResponse{}))
	stop := waitSpan(t, exp, telemetry.SpanAllocStop, string(telemetry.AttrAllocID), alloc.ID)
	must.False(t, stop.Parent.IsValid())
	must.MapContainsKey(t, childrenOf(exp, stop), telemetry.SpanTaskStop)

	// the garbage collection of the allocation: an alloc.gc trace
	must.True(t, c1.CollectAllocation(alloc.ID))
	gc := waitSpan(t, exp, telemetry.SpanAllocGC, string(telemetry.AttrAllocID), alloc.ID)
	must.False(t, gc.Parent.IsValid())
	must.MapContainsKey(t, childrenOf(exp, gc), "alloc.hook.alloc_dir")
}
