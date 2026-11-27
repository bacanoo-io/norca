// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The trace spans of the scheduling worker. The worker processes one
// evaluation at a time on its goroutine, and so does the scheduler it runs
// (its calls of SubmitPlan, UpdateEval, CreateEval and ReblockEval), so the
// trace state of the evaluation in progress lives on the Worker. Every
// helper is a no-op without an otlp block (s.config.Tracing nil).

// workerTrace is the trace state of the evaluation a worker processes.
type workerTrace struct {
	// span is the eval.process span, nil when the evaluation is not traced.
	span trace.Span

	// evalID is the ID of the evaluation of span.
	evalID string

	// ctx carries the innermost span of the evaluation: eval.process, or
	// scheduler.<type> while the scheduler runs. The server spans of the
	// worker's in-process RPCs are its children (traceParent).
	ctx         context.Context
	traceParent string

	// logger is the worker's logger with the trace IDs of span.
	logger hclog.Logger
}

// startEvalTrace starts the eval.process span of a dequeued evaluation, a
// child of traceParent (from the Eval.Dequeue reply) when there is one and
// otherwise a new root: an evaluation of an untraced creator, or one a new
// leader enqueued again. It returns the span context of the span (invalid
// without tracing).
func (w *Worker) startEvalTrace(eval *structs.Evaluation, traceParent string) trace.SpanContext {
	w.trace = workerTrace{logger: w.logger}
	tracing := w.srv.config.Tracing
	if tracing.Tracer() == nil {
		return trace.SpanContext{}
	}
	ctx := telemetry.ContextWithTraceParent(context.Background(), traceParent)
	ctx, span := tracing.Start(ctx, telemetry.SpanEvalProcess, trace.SpanKindInternal,
		telemetry.AttrEvalID.String(eval.ID),
		telemetry.AttrJobID.String(eval.JobID),
		telemetry.AttrNamespace.String(eval.Namespace),
		telemetry.AttrTriggeredBy.String(eval.TriggeredBy),
		telemetry.AttrSchedulerType.String(eval.Type),
	)
	sc := span.SpanContext()
	w.trace = workerTrace{
		span:        span,
		evalID:      eval.ID,
		ctx:         ctx,
		traceParent: telemetry.FormatTraceParent(sc),
		logger:      telemetry.LogWithSpanContext(w.logger, sc),
	}
	return sc
}

// failEvalTrace marks the eval.process span failed with err.
func (w *Worker) failEvalTrace(err error) {
	if w.trace.span != nil && err != nil {
		w.trace.span.SetStatus(codes.Error, err.Error())
	}
}

// endEvalTrace ends the eval.process span after the ack (or nack) of its
// evaluation; a nack is an error.
func (w *Worker) endEvalTrace(ack bool) {
	span := w.trace.span
	w.trace = workerTrace{}
	if span == nil {
		return
	}
	if !ack {
		span.SetStatus(codes.Error, "evaluation nacked")
	}
	span.End()
}

// evalStatusTrace records the status the scheduler gives the evaluation in
// process on its eval.process span.
func (w *Worker) evalStatusTrace(eval *structs.Evaluation) {
	if w.trace.span != nil && eval.ID == w.trace.evalID {
		w.trace.span.SetAttributes(telemetry.AttrEvalStatus.String(eval.Status))
	}
}

// startSchedulerTrace starts the scheduler.<type> span of a scheduler run
// within the eval.process span, and makes it the parent of the run's
// spans and RPCs until the returned function ends it with the run's error.
func (w *Worker) startSchedulerTrace(schedType string) func(error) {
	if w.trace.span == nil {
		return func(error) {}
	}
	parentCtx, parentTP := w.trace.ctx, w.trace.traceParent
	ctx, span := w.srv.config.Tracing.Start(parentCtx, telemetry.SpanSchedulerPrefix+schedType,
		trace.SpanKindInternal, telemetry.AttrSchedulerType.String(schedType))
	w.trace.ctx = ctx
	w.trace.traceParent = telemetry.FormatTraceParent(span.SpanContext())
	return func(err error) {
		w.trace.ctx, w.trace.traceParent = parentCtx, parentTP
		telemetry.EndSpan(span, err)
	}
}

// TraceContext returns the context of the span of the scheduler run in
// progress and the agent's tracer, for the scheduler's own spans
// (scheduler.TracingPlanner). Without a traced evaluation the tracer is nil.
func (w *Worker) TraceContext() (context.Context, trace.Tracer) {
	if w.trace.span == nil {
		return context.Background(), nil
	}
	return w.trace.ctx, w.srv.config.Tracing.Tracer()
}

// traceRPC sets the traceparent of the span in progress in the arguments of
// an in-process RPC of the worker, so that the RPC's server span is its
// child.
func (w *Worker) traceRPC(args telemetry.TraceCarrier) {
	if w.trace.traceParent != "" {
		args.SetTraceParent(w.trace.traceParent)
	}
}

// startPlanSubmitTrace starts the plan.submit span of a plan submission and
// returns the function that ends it with the submission's error, and the
// traceparent the Plan.Submit arguments carry ("" when not traced).
func (w *Worker) startPlanSubmitTrace(plan *structs.Plan) (func(error), string) {
	if w.trace.span == nil {
		return func(error) {}, ""
	}
	_, span := w.srv.config.Tracing.Start(w.trace.ctx, telemetry.SpanPlanSubmit, trace.SpanKindClient,
		telemetry.AttrEvalID.String(plan.EvalID))
	return func(err error) { telemetry.EndSpan(span, err) }, telemetry.FormatTraceParent(span.SpanContext())
}

// traceLogger returns the worker's logger with the trace IDs of the
// evaluation in process, if it is traced.
func (w *Worker) traceLogger() hclog.Logger {
	if w.trace.logger != nil {
		return w.trace.logger
	}
	return w.logger
}
