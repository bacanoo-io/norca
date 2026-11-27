// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics"
	"go.opentelemetry.io/otel"
)

// signalInternal labels the errors the OpenTelemetry SDK reports outside an
// export (dropped data, partial success, internal failures).
const signalInternal = "internal"

// errorLogInterval is how often one signal logs an export error at most.
const errorLogInterval = time.Minute

// exportErrorsKey is the go-metrics key of the export error counter, under
// the service prefix of the agent.
var exportErrorsKey = []string{"agent", "otlp", "export_errors"}

// exportError marks an error of the agent's own exporters, so the global
// OpenTelemetry error handler, which the SDK hands it to again, does not
// report it twice.
type exportError struct {
	signal string
	err    error
}

func (e *exportError) Error() string { return e.err.Error() }
func (e *exportError) Unwrap() error { return e.err }

// errorReporter logs the export errors at warn, one line per signal per
// interval with the number of errors since the last line, and counts every
// one in go-metrics, so they show in /v1/metrics even while OTLP is down.
// Credentials never reach it: the errors come from gRPC and the SDK, which
// do not quote metadata.
type errorReporter struct {
	logger   hclog.Logger
	now      func() time.Time
	interval time.Duration

	// quiet stops the log lines during the shutdown: a failing final flush
	// is reported once, to stderr, by the caller.
	quiet atomic.Bool

	mu     sync.Mutex
	states map[string]*errorState
}

type errorState struct {
	lastLog time.Time
	count   int
}

func newErrorReporter(logger hclog.Logger) *errorReporter {
	return &errorReporter{
		logger:   logger,
		now:      time.Now,
		interval: errorLogInterval,
		states:   map[string]*errorState{},
	}
}

// export reports a failed export of a signal.
func (r *errorReporter) export(signal string, err error) {
	metrics.IncrCounterWithLabels(exportErrorsKey, 1, []metrics.Label{{Name: "signal", Value: signal}})
	r.log(signal, err)
}

// log writes the error unless the signal logged one within the interval;
// the errors in between are counted into the next line.
func (r *errorReporter) log(signal string, err error) {
	if r.quiet.Load() {
		return
	}
	now := r.now()
	r.mu.Lock()
	st, ok := r.states[signal]
	if !ok {
		st = &errorState{}
		r.states[signal] = st
	}
	st.count++
	if !st.lastLog.IsZero() && now.Sub(st.lastLog) < r.interval {
		r.mu.Unlock()
		return
	}
	count := st.count
	st.count = 0
	st.lastLog = now
	r.mu.Unlock()

	logger := r.logger
	if feedsLogExport(signal) {
		logger = logger.With(LogKeyOTLPInternal, true)
	}
	logger.Warn("OTLP export failed", "signal", signal, "error", err, "count", count, "per", r.interval)
}

// feedsLogExport reports whether the error lines of a signal could feed the
// log export they report on: those of the logs signal and the SDK's internal
// errors, which the log SDK reports too. They carry LogKeyOTLPInternal, and
// the log sink drops them (the loop guard); the lines of the other signals
// are exported like any other warning.
func feedsLogExport(signal string) bool {
	return signal == SignalLogs || signal == signalInternal
}

// handle is the global OpenTelemetry error handler of the agent. The errors
// of the agent's exporters were reported already; any other error is logged
// with the same rate limit.
func (r *errorReporter) handle(err error) {
	if _, ok := errors.AsType[*exportError](err); ok {
		return
	}
	r.log(signalInternal, err)
}

// globalReporter is the reporter the global error handler forwards to: the
// one of the most recently started providers. The handler is installed once,
// because otel.SetErrorHandler only delegates on its first call.
var (
	globalReporter     atomic.Pointer[errorReporter]
	installHandlerOnce sync.Once
)

func installErrorHandler(r *errorReporter) {
	globalReporter.Store(r)
	installHandlerOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			if r := globalReporter.Load(); r != nil {
				r.handle(err)
			}
		}))
	})
}
