// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"context"
	"sync"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	log "github.com/hashicorp/go-hclog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The trace spans of the allocation runner. Each phase of an allocation on
// the client is a short trace of its own, never one span for the life of
// the allocation, and no trace context is stored in the allocation: the
// phases are joined to the scheduling trace and to each other by their
// norca.alloc.id, norca.eval.id and norca.job.id attributes.
//
//	alloc.start    the prerun hooks and the first start of every task (until
//	               each one started, failed to start or exited; poststop
//	               tasks are not waited for)
//	alloc.stop     a stop from the server: the prekill hooks, the kill of the
//	               tasks, and the postrun hooks once the tasks exited
//	alloc.restart  a restart of the tasks (API or check restarts)
//	alloc.gc       the destroy of the allocation: the kill of the tasks, the
//	               postrun and destroy hooks
//
// Below a phase: one alloc.hook.<name> span per hook, the task spans of the
// task runners, and the client spans of the RPCs the hooks make. Every
// helper is a no-op without an otlp block (clientConfig.Tracing nil).

// allocAttributes returns the attributes of the allocation's spans.
func (ar *allocRunner) allocAttributes() []attribute.KeyValue {
	alloc := ar.Alloc()
	return []attribute.KeyValue{
		telemetry.AttrAllocID.String(alloc.ID),
		telemetry.AttrEvalID.String(alloc.EvalID),
		telemetry.AttrJobID.String(alloc.JobID),
		telemetry.AttrNamespace.String(alloc.Namespace),
		telemetry.AttrTaskGroup.String(alloc.TaskGroup),
	}
}

// startPhase starts the span of a phase of the allocation, the root of a
// new trace (the phases are always sampled), and makes it the phase in
// progress. Without tracing it returns a context without a span and a span
// that records nothing.
func (ar *allocRunner) startPhase(ctx context.Context, name string) (context.Context, trace.Span) {
	tracing := ar.clientConfig.Tracing
	if tracing.Tracer() == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	ctx, span := tracing.Start(ctx, name, trace.SpanKindInternal, ar.allocAttributes()...)
	ar.phase.Set(ctx, span)
	return ctx, span
}

// startHookSpan starts the span of an allocation runner hook within the
// phase in progress; outside a phase it records nothing.
func (ar *allocRunner) startHookSpan(name string) trace.Span {
	_, span := ar.clientConfig.Tracing.StartIn(ar.phase.Context(), telemetry.SpanAllocHookPrefix+name, trace.SpanKindInternal)
	return span
}

// phaseLogger returns the runner's logger with the trace IDs of the phase in
// progress, if any.
func (ar *allocRunner) phaseLogger() log.Logger {
	return telemetry.LogWith(ar.logger, ar.phase.Context())
}

// startTasksTrace hands the alloc.start span to the task runners for their
// first start, and ends the span once every task that is not a poststop
// task finished its first start attempt (or its runner exited).
func (ar *allocRunner) startTasksTrace(ctx context.Context, span trace.Span) {
	if !span.IsRecording() {
		return
	}
	var wg sync.WaitGroup
	for _, tr := range ar.tasks {
		if tr.IsPoststopTask() {
			continue
		}
		wg.Add(1)
		tr.TraceNextStart(ctx, wg.Done)
	}
	go func() {
		wg.Wait()
		span.End()
	}()
}
