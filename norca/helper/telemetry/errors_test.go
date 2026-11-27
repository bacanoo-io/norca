// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
)

// syncBuffer is a log output the test can read while the reporter writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines(substr string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

func testLogger(out *syncBuffer) hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{Output: out, Level: hclog.Trace})
}

func TestErrorReporter_RateLimit(t *testing.T) {
	ci.Parallel(t)

	var out syncBuffer
	r := newErrorReporter(testLogger(&out))
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	errDown := errors.New("connection refused")
	r.log(SignalMetrics, errDown)
	r.log(SignalTraces, errDown)
	for range 5 {
		now = now.Add(10 * time.Second)
		r.log(SignalMetrics, errDown)
	}
	lines := out.lines("OTLP export failed")
	must.Len(t, 2, lines, must.Sprint("one line per signal within the minute"))
	must.StrContains(t, lines[0], "signal=metrics")
	must.StrContains(t, lines[0], "count=1")
	must.StrContains(t, lines[1], "signal=traces")

	// the minute is over: the next error logs with the count since the
	// last line
	now = now.Add(10 * time.Second)
	r.log(SignalMetrics, errDown)
	lines = out.lines("signal=metrics")
	must.Len(t, 2, lines)
	must.StrContains(t, lines[1], "count=6")

	// quiet during the shutdown
	r.quiet.Store(true)
	now = now.Add(time.Hour)
	r.log(SignalMetrics, errDown)
	must.Len(t, 2, out.lines("signal=metrics"))
}

func TestErrorReporter_Handle(t *testing.T) {
	ci.Parallel(t)

	var out syncBuffer
	r := newErrorReporter(testLogger(&out))

	// errors of the agent's exporters were reported already
	r.handle(fmt.Errorf("wrapped: %w", &exportError{signal: SignalTraces, err: errors.New("down")}))
	must.SliceEmpty(t, out.lines("OTLP export failed"))

	r.handle(errors.New("partial success"))
	lines := out.lines("OTLP export failed")
	must.Len(t, 1, lines)
	must.StrContains(t, lines[0], "signal=internal")
}

func TestExportError(t *testing.T) {
	ci.Parallel(t)

	inner := errors.New("down")
	err := error(&exportError{signal: SignalMetrics, err: inner})
	must.EqError(t, err, "down")
	must.ErrorIs(t, err, inner)
}
