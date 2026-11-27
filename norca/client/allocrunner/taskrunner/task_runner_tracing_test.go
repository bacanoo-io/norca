// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/pluginmanager/drivermanager"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// ctxDriverManager hands out the drivers of a manager wrapped in ctxDriver.
type ctxDriverManager struct {
	drivermanager.Manager
	tracer trace.Tracer
}

func (m *ctxDriverManager) Dispense(name string) (drivers.DriverPlugin, error) {
	d, err := m.Manager.Dispense(name)
	if err != nil {
		return nil, err
	}
	return &ctxDriver{DriverPlugin: d, tracer: m.tracer}, nil
}

// ctxDriver is a driver that starts tasks within a context and records the
// start as a client span, as the gRPC client of a driver plugin does.
type ctxDriver struct {
	drivers.DriverPlugin
	tracer trace.Tracer
}

func (d *ctxDriver) StartTaskContext(ctx context.Context, cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	_, span := d.tracer.Start(ctx, "hashicorp.nomad.plugins.drivers.proto.Driver/StartTask",
		trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	return d.DriverPlugin.StartTask(cfg)
}

// A start attempt handed a parent is a task.start span with the task's
// hooks and the driver's StartTask call below it; done is called once the
// attempt finished. A kill within a phase is a task.stop span.
func TestTaskRunner_TracingStart(t *testing.T) {
	ci.Parallel(t)

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracing := telemetry.NewTracing(tp, telemetry.TracesSettings{})

	alloc := mock.BatchAlloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Driver = "mock_driver"
	task.Config = map[string]any{"run_for": "10s"}

	conf, cleanup := testTaskRunnerConfig(t, alloc, task.Name)
	t.Cleanup(cleanup)
	conf.ClientConfig.Tracing = tracing
	conf.DriverManager = &ctxDriverManager{Manager: conf.DriverManager, tracer: tracing.Tracer()}

	tr, err := NewTaskRunner(conf)
	must.NoError(t, err)

	phaseCtx, phase := tracing.Tracer().Start(context.Background(), telemetry.SpanAllocStart)
	done := make(chan struct{})
	tr.TraceNextStart(phaseCtx, func() { close(done) })
	go tr.Run()
	t.Cleanup(func() { _ = tr.Kill(context.Background(), structs.NewTaskEvent("cleanup")) })

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the start attempt did not finish")
	}
	phase.End()

	spans := exp.GetSpans()
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	start, ok := byName[telemetry.SpanTaskStart]
	must.True(t, ok)
	must.Eq(t, phase.SpanContext().SpanID(), start.Parent.SpanID())
	must.Eq(t, alloc.ID, spanAttr(start, telemetry.AttrAllocID))
	must.Eq(t, alloc.EvalID, spanAttr(start, telemetry.AttrEvalID))
	must.Eq(t, task.Name, spanAttr(start, telemetry.AttrTask))

	driverStart := byName["hashicorp.nomad.plugins.drivers.proto.Driver/StartTask"]
	must.Eq(t, start.SpanContext.SpanID(), driverStart.Parent.SpanID(), must.Sprint("StartTask below task.start"))
	for _, hook := range []string{"task.hook.validate", "task.hook.task_dir"} {
		s, ok := byName[hook]
		must.True(t, ok, must.Sprint(hook))
		must.Eq(t, start.SpanContext.SpanID(), s.Parent.SpanID())
	}

	// a kill within a phase
	stopCtx, stopPhase := tracing.Tracer().Start(context.Background(), telemetry.SpanAllocStop)
	must.NoError(t, tr.Kill(stopCtx, structs.NewTaskEvent(structs.TaskKilling)))
	stopPhase.End()
	var stop tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == telemetry.SpanTaskStop {
			stop = s
		}
	}
	must.Eq(t, stopPhase.SpanContext().SpanID(), stop.Parent.SpanID())
}

// Without a parent the start attempt is not traced, and a runner that
// exits before its start calls done.
func TestTaskRunner_TracingUntraced(t *testing.T) {
	ci.Parallel(t)

	tr := &TaskRunner{}
	done := make(chan struct{})
	tr.TraceNextStart(context.Background(), func() { close(done) })
	tr.finishStartTrace()
	select {
	case <-done:
	default:
		t.Fatal("done not called")
	}
	ctx, span := tr.startPhaseSpan(nil, telemetry.SpanTaskStart)
	must.False(t, span.IsRecording())
	must.False(t, trace.SpanContextFromContext(ctx).IsValid())
}

func spanAttr(s tracetest.SpanStub, key attribute.Key) string {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value.Emit()
		}
	}
	return ""
}
