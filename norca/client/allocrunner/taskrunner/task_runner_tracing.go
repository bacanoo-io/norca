// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"sync"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The trace spans of the task runner, below the phases of the allocation
// runner (see the allocrunner package):
//
//	task.start    one start attempt: the prestart hooks, the driver's
//	              StartTask (with the driver plugin's client span below it)
//	              and the poststart hooks; traced when the allocation runner
//	              (alloc.start) or a restart (task.restart) handed it a
//	              parent, so restarts by the restart policy are not traced
//	task.stop     a kill of the task within a phase (alloc.stop, alloc.gc)
//	task.restart  a restart within alloc.restart
//
// with task.hook.<name> per hook and artifact.download per artifact. Every
// helper is a no-op without an otlp block (clientConfig.Tracing nil).

// startTrace is the parent of the task's next task.start span.
type startTrace struct {
	mu     sync.Mutex
	parent context.Context
	done   func()
}

// TraceNextStart makes the span of ctx the parent of the task's next start
// attempt; done is called once that attempt finished, or when the runner
// exits without one. The allocation runner hands its alloc.start span this
// way.
func (tr *TaskRunner) TraceNextStart(ctx context.Context, done func()) {
	tr.startTrace.mu.Lock()
	prevDone := tr.startTrace.done
	tr.startTrace.parent, tr.startTrace.done = ctx, done
	tr.startTrace.mu.Unlock()
	if prevDone != nil {
		prevDone()
	}
}

// takeStartTrace returns the parent of this start attempt and its done
// callback (nil when the start is not traced), and clears them.
func (tr *TaskRunner) takeStartTrace() (context.Context, func()) {
	tr.startTrace.mu.Lock()
	defer tr.startTrace.mu.Unlock()
	parent, done := tr.startTrace.parent, tr.startTrace.done
	tr.startTrace.parent, tr.startTrace.done = nil, nil
	return parent, done
}

// finishStartTrace calls the done callback of a start that never happened,
// when the runner exits.
func (tr *TaskRunner) finishStartTrace() {
	if _, done := tr.takeStartTrace(); done != nil {
		done()
	}
}

// taskAttributes returns the attributes of the task's spans.
func (tr *TaskRunner) taskAttributes() []attribute.KeyValue {
	alloc := tr.Alloc()
	return []attribute.KeyValue{
		telemetry.AttrAllocID.String(alloc.ID),
		telemetry.AttrEvalID.String(alloc.EvalID),
		telemetry.AttrJobID.String(alloc.JobID),
		telemetry.AttrNamespace.String(alloc.Namespace),
		telemetry.AttrTaskGroup.String(alloc.TaskGroup),
		telemetry.AttrTask.String(tr.taskName),
	}
}

// startPhaseSpan starts a span of the task named name as a child of the span
// in ctx and makes it the task's phase in progress, which the hooks, the
// driver call and the RPCs of the task join. Without a span in ctx it
// records nothing.
func (tr *TaskRunner) startPhaseSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if ctx == nil || !trace.SpanContextFromContext(ctx).IsValid() {
		return context.Background(), trace.SpanFromContext(context.Background())
	}
	ctx, span := tr.clientConfig.Tracing.StartIn(ctx, name, trace.SpanKindInternal, tr.taskAttributes()...)
	if span.IsRecording() {
		tr.phase.Set(ctx, span)
	}
	return ctx, span
}

// startTaskStart starts the task.start span of a start attempt, and returns
// the function that ends it with the attempt's error.
func (tr *TaskRunner) startTaskStart() (context.Context, func(error)) {
	parent, done := tr.takeStartTrace()
	ctx, span := tr.startPhaseSpan(parent, telemetry.SpanTaskStart)
	return ctx, func(err error) {
		telemetry.EndSpan(span, err)
		if done != nil {
			done()
		}
	}
}

// hookSpan starts the span of a task runner hook within the task's phase in
// progress and returns ctx with it for the hook; outside a phase it returns
// ctx and a span that records nothing.
func (tr *TaskRunner) hookSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	_, span := tr.clientConfig.Tracing.StartIn(tr.phase.Context(), telemetry.SpanTaskHookPrefix+name, trace.SpanKindInternal)
	if !span.IsRecording() {
		return ctx, span
	}
	return trace.ContextWithSpan(ctx, span), span
}

// startDriverTask starts the task with the driver, within the task's phase
// in progress when the driver takes a context (an external driver plugin,
// whose gRPC client traces the call).
func (tr *TaskRunner) startDriverTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if d, ok := tr.driver.(drivers.TaskStarterContext); ok {
		return d.StartTaskContext(tr.phase.Context(), cfg)
	}
	return tr.driver.StartTask(cfg)
}
