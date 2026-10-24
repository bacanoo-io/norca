// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reconcile results reported to Metrics.
const (
	ResultSuccess = "success"
	ResultRequeue = "requeue"
	ResultError   = "error"
)

// Metrics receives the measurements of a controller. Implementations must be
// safe for concurrent use. Embed NopMetrics to implement only some methods.
type Metrics interface {
	// ReconcileDone is called after every Reconcile call with its duration
	// and result (ResultSuccess, ResultRequeue or ResultError).
	ReconcileDone(controller string, d time.Duration, result string)

	// QueueDepth reports the number of keys ready for processing.
	QueueDepth(controller string, depth int)

	// Resync is called when a watch had to list again, because the server
	// could not resume the event stream (410 Gone), on start, or
	// periodically.
	Resync(controller, source, reason string)

	// EventReceived is called for every event received from the event
	// stream.
	EventReceived(controller, topic string)

	// LeaderChanged is called when the controller gains or loses the
	// leadership.
	LeaderChanged(controller string, leader bool)
}

// NopMetrics discards every measurement.
type NopMetrics struct{}

func (NopMetrics) ReconcileDone(string, time.Duration, string) {}
func (NopMetrics) QueueDepth(string, int)                      {}
func (NopMetrics) Resync(string, string, string)               {}
func (NopMetrics) EventReceived(string, string)                {}
func (NopMetrics) LeaderChanged(string, bool)                  {}

// TextMetrics keeps the measurements in memory and serves them in the
// Prometheus text exposition format, so a controller can expose /metrics
// without further dependencies.
type TextMetrics struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]float64
}

// NewTextMetrics returns an empty TextMetrics.
func NewTextMetrics() *TextMetrics {
	return &TextMetrics{counters: map[string]float64{}, gauges: map[string]float64{}}
}

func series(name string, labels ...string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", labels[i], labels[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

func (m *TextMetrics) add(key string, v float64) {
	m.mu.Lock()
	m.counters[key] += v
	m.mu.Unlock()
}

func (m *TextMetrics) set(key string, v float64) {
	m.mu.Lock()
	m.gauges[key] = v
	m.mu.Unlock()
}

func (m *TextMetrics) ReconcileDone(controller string, d time.Duration, result string) {
	m.add(series("norca_controller_reconcile_total", "controller", controller, "result", result), 1)
	m.add(series("norca_controller_reconcile_seconds_sum", "controller", controller), d.Seconds())
	m.add(series("norca_controller_reconcile_seconds_count", "controller", controller), 1)
}

func (m *TextMetrics) QueueDepth(controller string, depth int) {
	m.set(series("norca_controller_queue_depth", "controller", controller), float64(depth))
}

func (m *TextMetrics) Resync(controller, source, reason string) {
	m.add(series("norca_controller_resync_total", "controller", controller, "source", source, "reason", reason), 1)
}

func (m *TextMetrics) EventReceived(controller, topic string) {
	m.add(series("norca_controller_events_total", "controller", controller, "topic", topic), 1)
}

func (m *TextMetrics) LeaderChanged(controller string, leader bool) {
	v := 0.0
	if leader {
		v = 1
	}
	m.set(series("norca_controller_leader", "controller", controller), v)
}

// Value returns the current value of a series, for tests and debugging.
func (m *TextMetrics) Value(name string, labels ...string) float64 {
	key := series(name, labels...)
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.counters[key]; ok {
		return v
	}
	return m.gauges[key]
}

// WriteTo writes the metrics in the Prometheus text format.
func (m *TextMetrics) WriteTo(w io.Writer) (int64, error) {
	m.mu.Lock()
	lines := make([]string, 0, len(m.counters)+len(m.gauges))
	for k, v := range m.counters {
		lines = append(lines, fmt.Sprintf("%s %g", k, v))
	}
	for k, v := range m.gauges {
		lines = append(lines, fmt.Sprintf("%s %g", k, v))
	}
	m.mu.Unlock()
	sort.Strings(lines)
	n, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return int64(n), err
}

// ServeHTTP serves the metrics in the Prometheus text format.
func (m *TextMetrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = m.WriteTo(w)
}
