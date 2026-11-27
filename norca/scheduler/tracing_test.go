// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// tracingHarness is the test harness as a planner whose runs are traced,
// below the span in ctx.
type tracingHarness struct {
	*Harness
	ctx    context.Context
	tracer trace.Tracer
}

func (h *tracingHarness) TraceContext() (context.Context, trace.Tracer) { return h.ctx, h.tracer }

// runTraced processes eval with the scheduler of factory as a traced run
// and returns the spans it recorded below the run's span.
func runTraced(t *testing.T, h *Harness, factory Factory, eval *structs.Evaluation) []tracetest.SpanStub {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer(telemetry.InstrumentationName)

	ctx, run := tracer.Start(context.Background(), "scheduler.test")
	planner := &tracingHarness{Harness: h, ctx: ctx, tracer: tracer}
	sched := factory(testlog.HCLogger(t), make(chan any, 16), h.Snapshot(), planner)
	must.NoError(t, sched.Process(eval))
	run.End()
	return exp.GetSpans()
}

func spanNamed(t *testing.T, spans []tracetest.SpanStub, name string) []tracetest.SpanStub {
	t.Helper()
	var out []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func spanAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

func registerEval(t *testing.T, h *Harness, job *structs.Job) *structs.Evaluation {
	t.Helper()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		Type:        job.Type,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	return eval
}

// A traced service scheduler run has a scheduler.compute span with one
// scheduler.select span per task group below it, which counts the
// placements made and failed, and no span per node.
func TestSchedulerTracing_Generic(t *testing.T) {
	ci.Parallel(t)
	h := NewHarness(t)
	_ = createNodes(t, h, 2)

	job := mock.Job()
	job.TaskGroups[0].Count = 3
	// a group no node can take
	big := job.TaskGroups[0].Copy()
	big.Name = "big"
	big.Count = 2
	big.Tasks[0].Resources.MemoryMB = 1 << 30
	job.TaskGroups = append(job.TaskGroups, big)

	spans := runTraced(t, h, NewServiceScheduler, registerEval(t, h, job))
	computes := spanNamed(t, spans, telemetry.SpanSchedulerCompute)
	must.SliceNotEmpty(t, computes)
	selects := spanNamed(t, spans, telemetry.SpanSchedulerSelect)
	byGroup := map[string]tracetest.SpanStub{}
	for _, s := range selects {
		byGroup[spanAttr(s, "norca.task_group")] = s
	}
	must.MapLen(t, 2, byGroup)
	must.Eq(t, "3", spanAttr(byGroup["web"], "norca.scheduler.placed"))
	must.Eq(t, "0", spanAttr(byGroup["web"], "norca.scheduler.failed"))
	must.Eq(t, "0", spanAttr(byGroup["big"], "norca.scheduler.placed"))
	must.Eq(t, "2", spanAttr(byGroup["big"], "norca.scheduler.failed"))
	must.Eq(t, computes[0].SpanContext.SpanID(), byGroup["web"].Parent.SpanID())
}

// A traced system scheduler run has one scheduler.select span per task
// group, not per node.
func TestSchedulerTracing_System(t *testing.T) {
	ci.Parallel(t)
	h := NewHarness(t)
	_ = createNodes(t, h, 5)

	spans := runTraced(t, h, NewSystemScheduler, registerEval(t, h, mock.SystemJob()))
	must.Len(t, 1, spanNamed(t, spans, telemetry.SpanSchedulerCompute))
	selects := spanNamed(t, spans, telemetry.SpanSchedulerSelect)
	must.Len(t, 1, selects)
	must.Eq(t, "5", spanAttr(selects[0], "norca.scheduler.placed"))
}

// An untraced planner (the plain harness) gives the scheduler the zero
// tracer, which starts nothing.
func TestSchedulerTracing_Untraced(t *testing.T) {
	ci.Parallel(t)
	h := NewHarness(t)
	must.Eq(t, schedTracer{}, newSchedTracer(h))

	sel := newSelectSpans(schedTracer{})
	sel.placed("web")
	sel.failed("web")
	sel.end()
	must.Nil(t, sel.groups)
}
