// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics keeps the measurements of a bridge and serves them in the
// Prometheus text exposition format. It is safe for concurrent use.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]float64

	// pendingSince is when the oldest batch received but not yet
	// published was received; zero if there is none.
	pendingSince time.Time
	now          func() time.Time
}

// NewMetrics returns empty metrics.
func NewMetrics() *Metrics {
	return &Metrics{counters: map[string]float64{}, gauges: map[string]float64{}, now: time.Now}
}

func series(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
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

func (m *Metrics) add(v float64, name string, labels ...string) {
	if m == nil {
		return
	}
	key := series(name, labels...)
	m.mu.Lock()
	m.counters[key] += v
	m.mu.Unlock()
}

func (m *Metrics) set(v float64, name string, labels ...string) {
	if m == nil {
		return
	}
	key := series(name, labels...)
	m.mu.Lock()
	m.gauges[key] = v
	m.mu.Unlock()
}

// Metric names.
const (
	MetricPublished          = "norca_natsbridge_events_published_total"
	MetricDuplicates         = "norca_natsbridge_events_duplicate_total"
	MetricSkipped            = "norca_natsbridge_events_skipped_total"
	MetricPublishFailures    = "norca_natsbridge_publish_failures_total"
	MetricStreamErrors       = "norca_natsbridge_stream_errors_total"
	MetricGaps               = "norca_natsbridge_gaps_total"
	MetricCheckpointFailures = "norca_natsbridge_checkpoint_failures_total"
	MetricReceivedIndex      = "norca_natsbridge_received_index"
	MetricPublishedIndex     = "norca_natsbridge_published_index"
	MetricCheckpointIndex    = "norca_natsbridge_checkpoint_index"
	MetricLagIndex           = "norca_natsbridge_lag_index"
	MetricLagSeconds         = "norca_natsbridge_lag_seconds"
	MetricLeader             = "norca_natsbridge_leader"
)

func (m *Metrics) published(topic string, n, duplicates int) {
	m.add(float64(n), MetricPublished, "topic", topic)
	if duplicates > 0 {
		m.add(float64(duplicates), MetricDuplicates)
	}
}

func (m *Metrics) skipped(reason string) { m.add(1, MetricSkipped, "reason", reason) }

func (m *Metrics) publishFailed(n int) { m.add(float64(n), MetricPublishFailures) }

func (m *Metrics) streamError(reason string) { m.add(1, MetricStreamErrors, "reason", reason) }

func (m *Metrics) gap() { m.add(1, MetricGaps) }

func (m *Metrics) checkpointFailed() { m.add(1, MetricCheckpointFailures) }

func (m *Metrics) checkpointed(index uint64) { m.set(float64(index), MetricCheckpointIndex) }

// received records a batch received from the stream and not yet published.
func (m *Metrics) received(index uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingSince.IsZero() {
		m.pendingSince = m.now()
	}
	m.gauges[MetricReceivedIndex] = float64(index)
}

// publishedIndex records that every batch up to index is published.
func (m *Metrics) publishedIndex(index uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingSince = time.Time{}
	m.gauges[MetricPublishedIndex] = float64(index)
	if m.gauges[MetricReceivedIndex] < float64(index) {
		m.gauges[MetricReceivedIndex] = float64(index)
	}
}

// SetLeader records whether this instance is the leader.
func (m *Metrics) SetLeader(leader bool) {
	v := 0.0
	if leader {
		v = 1
	}
	m.set(v, MetricLeader)
}

// Value returns the current value of a series, for tests and debugging.
func (m *Metrics) Value(name string, labels ...string) float64 {
	key := series(name, labels...)
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.counters[key]; ok {
		return v
	}
	return m.gauges[key]
}

// lag computes the derived gauges: the index distance and the time since
// the oldest batch that was received but is not published yet (0 when the
// bridge has caught up). While NATS is unreachable both grow.
func (m *Metrics) lagLocked() (index, seconds float64) {
	index = max(m.gauges[MetricReceivedIndex]-m.gauges[MetricPublishedIndex], 0)
	if !m.pendingSince.IsZero() {
		seconds = m.now().Sub(m.pendingSince).Seconds()
	}
	return index, seconds
}

// WriteTo writes the metrics in the Prometheus text format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	m.mu.Lock()
	lines := make([]string, 0, len(m.counters)+len(m.gauges)+2)
	for k, v := range m.counters {
		lines = append(lines, fmt.Sprintf("%s %g", k, v))
	}
	for k, v := range m.gauges {
		lines = append(lines, fmt.Sprintf("%s %g", k, v))
	}
	lagIndex, lagSeconds := m.lagLocked()
	m.mu.Unlock()
	lines = append(lines,
		fmt.Sprintf("%s %g", MetricLagIndex, lagIndex),
		fmt.Sprintf("%s %g", MetricLagSeconds, lagSeconds))
	sort.Strings(lines)
	n, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return int64(n), err
}

// ServeHTTP serves the metrics in the Prometheus text format.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = m.WriteTo(w)
}
