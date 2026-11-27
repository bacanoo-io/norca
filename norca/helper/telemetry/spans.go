// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span names of the scheduling of an evaluation on the servers.
const (
	// SpanEvalProcess is the worker's processing of one evaluation, from
	// the dequeue to the ack or nack; a child of the span that created the
	// evaluation when the leader knows it.
	SpanEvalProcess = "eval.process"

	// SpanSchedulerPrefix is the prefix of the span of a scheduler run,
	// named after the scheduler: scheduler.service, scheduler.batch,
	// scheduler.system, scheduler.sysbatch, scheduler._core.
	SpanSchedulerPrefix = "scheduler."

	// SpanSchedulerCompute is the scheduler's reconciliation of the job's
	// allocations with its desired state, placements included.
	SpanSchedulerCompute = "scheduler.compute"

	// SpanSchedulerSelect is the selection of nodes for the placements of
	// one task group.
	SpanSchedulerSelect = "scheduler.select"

	// SpanPlanSubmit is the worker's submission of a plan (the client side
	// of Plan.Submit).
	SpanPlanSubmit = "plan.submit"

	// SpanPlanEvaluate is the leader's evaluation of a plan against the
	// state; the gap between Plan.Submit and it is the plan queue wait.
	SpanPlanEvaluate = "plan.evaluate"

	// SpanPlanApply is the leader's application of an evaluated plan: its
	// Raft apply, until the entry is committed.
	SpanPlanApply = "plan.apply"
)

// Span names of the allocation lifecycle on the clients, below the phase
// spans (SpanAllocStart, ...).
const (
	// SpanAllocHookPrefix prefixes the span of an allocation runner hook:
	// alloc.hook.<hook name>.
	SpanAllocHookPrefix = "alloc.hook."

	// SpanTaskHookPrefix prefixes the span of a task runner hook:
	// task.hook.<hook name>.
	SpanTaskHookPrefix = "task.hook."

	// SpanTaskStart is one attempt to start a task: its prestart hooks,
	// the driver's StartTask and its poststart hooks.
	SpanTaskStart = "task.start"

	// SpanTaskStop is the kill of a task.
	SpanTaskStop = "task.stop"

	// SpanTaskRestart is the restart of a task.
	SpanTaskRestart = "task.restart"

	// SpanArtifactDownload is the download of one artifact of a task.
	SpanArtifactDownload = "artifact.download"
)

// Span attributes that join the traces of the scheduling and of the
// allocation phases on the clients: a client's alloc.start trace has the
// norca.eval.id of the eval.process trace that placed the allocation.
const (
	AttrEvalID      = attribute.Key("norca.eval.id")
	AttrJobID       = attribute.Key("norca.job.id")
	AttrNamespace   = attribute.Key("norca.namespace")
	AttrAllocID     = attribute.Key("norca.alloc.id")
	AttrTaskGroup   = attribute.Key("norca.task_group")
	AttrTask        = attribute.Key("norca.task")
	AttrTriggeredBy = attribute.Key("norca.eval.triggered_by")

	// AttrEvalStatus is the status the scheduler gave the evaluation.
	AttrEvalStatus = attribute.Key("norca.eval.status")

	// AttrSchedulerType is the scheduler of an evaluation (its type).
	AttrSchedulerType = attribute.Key("norca.scheduler.type")

	// AttrPlaced and AttrFailed count the placements a scheduler made and
	// the ones it could not make (coalesced failures included).
	AttrPlaced = attribute.Key("norca.scheduler.placed")
	AttrFailed = attribute.Key("norca.scheduler.failed")
)

// noopSpan is a span that records nothing, for code that ends spans
// unconditionally.
var noopSpan = trace.SpanFromContext(context.Background())

// Start starts a span named name as a child of the span in ctx, and returns
// the context with the new span. Without tracing (t nil or not bound yet)
// it starts nothing: it returns ctx and a span that records nothing.
func (t *Tracing) Start(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	tracer := t.Tracer()
	if tracer == nil {
		return ctx, noopSpan
	}
	return tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
}

// StartIn starts a span named name as a child of the span in ctx only when
// ctx carries one: code that runs both inside and outside a traced phase
// joins the trace of the phase and starts no root of its own. It returns
// ctx and a span that records nothing otherwise.
func (t *Tracing) StartIn(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, noopSpan
	}
	return t.Start(ctx, name, kind, attrs...)
}

// EndSpan ends span with an error status when err is not nil.
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// SpanTracker holds the span of the phase in progress of a runner on a
// client (an allocation or a task), which the runner's hooks and RPCs
// join. When phases overlap the latest one wins. The zero SpanTracker has
// no phase. It is safe for concurrent use.
type SpanTracker struct {
	current atomic.Pointer[trackedSpan]
}

type trackedSpan struct {
	ctx  context.Context
	span trace.Span
}

// Set makes span, whose context is ctx, the phase in progress.
func (p *SpanTracker) Set(ctx context.Context, span trace.Span) {
	p.current.Store(&trackedSpan{ctx: ctx, span: span})
}

// Context returns the context of the phase in progress, or a context
// without a span when no phase is in progress (or the last one ended).
func (p *SpanTracker) Context() context.Context {
	if cur := p.current.Load(); cur != nil && cur.span.IsRecording() {
		return cur.ctx
	}
	return context.Background()
}
