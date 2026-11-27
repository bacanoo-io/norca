// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"context"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TracingPlanner is a Planner whose scheduler runs are traced: the server's
// worker, which runs a scheduler within its scheduler.<type> span. The
// scheduler adds its own spans below it: scheduler.compute, and
// scheduler.select per task group. A Planner without the method (the test
// harness) is not traced.
type TracingPlanner interface {
	// TraceContext returns the context of the span of the scheduler run
	// and the tracer of the agent; a nil tracer when the run is not traced.
	TraceContext() (context.Context, trace.Tracer)
}

// schedTracer starts the spans of a scheduler run. The zero schedTracer
// traces nothing and allocates nothing.
type schedTracer struct {
	ctx    context.Context
	tracer trace.Tracer
}

// newSchedTracer returns the tracer of a scheduler run for planner.
func newSchedTracer(planner Planner) schedTracer {
	p, ok := planner.(TracingPlanner)
	if !ok {
		return schedTracer{}
	}
	ctx, tracer := p.TraceContext()
	if tracer == nil || ctx == nil {
		return schedTracer{}
	}
	return schedTracer{ctx: ctx, tracer: tracer}
}

// start starts a span named name as a child of the tracer's span and
// returns the tracer of the new span with it. Without tracing it returns
// the zero schedTracer and a span that records nothing.
func (t schedTracer) start(name string, attrs ...attribute.KeyValue) (schedTracer, trace.Span) {
	if t.tracer == nil {
		return schedTracer{}, trace.SpanFromContext(context.Background())
	}
	ctx, span := t.tracer.Start(t.ctx, name, trace.WithAttributes(attrs...))
	return schedTracer{ctx: ctx, tracer: t.tracer}, span
}

// selectSpans traces the node selection of a scheduler run: one
// scheduler.select span per task group, from its first placement to the
// end of the run's placements, with the counts of its placements made and
// failed (failures coalesced into an earlier one of the group included).
// It never starts a span per node considered. The zero selectSpans, of an
// untraced run, does nothing.
type selectSpans struct {
	parent schedTracer
	groups map[string]*selectSpan
}

type selectSpan struct {
	span           trace.Span
	placed, failed int
}

func newSelectSpans(parent schedTracer) selectSpans {
	return selectSpans{parent: parent}
}

// group returns the span state of a task group, starting its span at the
// group's first placement; nil without tracing.
func (s *selectSpans) group(tg string) *selectSpan {
	if s.parent.tracer == nil {
		return nil
	}
	if g, ok := s.groups[tg]; ok {
		return g
	}
	if s.groups == nil {
		s.groups = make(map[string]*selectSpan)
	}
	_, span := s.parent.start(telemetry.SpanSchedulerSelect, telemetry.AttrTaskGroup.String(tg))
	g := &selectSpan{span: span}
	s.groups[tg] = g
	return g
}

// placed counts a placement of task group tg.
func (s *selectSpans) placed(tg string) {
	if g := s.group(tg); g != nil {
		g.placed++
	}
}

// failed counts a placement of task group tg that found no node.
func (s *selectSpans) failed(tg string) {
	if g := s.group(tg); g != nil {
		g.failed++
	}
}

// end ends the span of every task group with its counts.
func (s *selectSpans) end() {
	for _, g := range s.groups {
		g.span.SetAttributes(telemetry.AttrPlaced.Int(g.placed), telemetry.AttrFailed.Int(g.failed))
		g.span.End()
	}
	s.groups = nil
}
