// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/nomad/internal/tstorage"
)

// The metrics the servers sample from their state and from the live usage
// of the nodes, apart from the task groups. Each is stored as <name>.avg and
// <name>.max with the same value, the last sampled in the interval.
const (
	// The capacity of the ready nodes of a datacenter and node pool
	// (labels dc, pool), in percent of the capacity left after the reserved
	// resources: allocated by the running allocations, and actually used
	// as the clients report it.
	MetricClusterCPUAllocated    = "norca.cluster.cpu_allocated"
	MetricClusterCPUUsed         = "norca.cluster.cpu_used"
	MetricClusterMemoryAllocated = "norca.cluster.memory_allocated"
	MetricClusterMemoryUsed      = "norca.cluster.memory_used"
	MetricClusterDiskAllocated   = "norca.cluster.disk_allocated"
	// MetricClusterNodesReady is the number of ready, eligible nodes of a
	// datacenter and node pool.
	MetricClusterNodesReady = "norca.cluster.nodes_ready"

	// MetricAutopilotHealthy (1 healthy, 0 not) and
	// MetricAutopilotFailureTolerance are the health of the servers; only
	// the leader has them.
	MetricAutopilotHealthy          = "norca.cluster.autopilot_healthy"
	MetricAutopilotFailureTolerance = "norca.cluster.autopilot_failure_tolerance"

	// The resources of the namespaces of a quota (label quota) in the
	// region: allocated to their allocations (MHz, MiB), and actually used
	// as the clients report it.
	MetricQuotaCPUAllocated    = "norca.quota.cpu_mhz_allocated"
	MetricQuotaCPUUsed         = "norca.quota.cpu_mhz_used"
	MetricQuotaMemoryAllocated = "norca.quota.memory_mb_allocated"
	MetricQuotaMemoryUsed      = "norca.quota.memory_mb_used"
)

// gaugeLabels are the labels of the gauge metrics, by metric.
var gaugeLabels = map[string][]string{
	MetricClusterCPUAllocated:       {"dc", "pool"},
	MetricClusterCPUUsed:            {"dc", "pool"},
	MetricClusterMemoryAllocated:    {"dc", "pool"},
	MetricClusterMemoryUsed:         {"dc", "pool"},
	MetricClusterDiskAllocated:      {"dc", "pool"},
	MetricClusterNodesReady:         {"dc", "pool"},
	MetricAutopilotHealthy:          {},
	MetricAutopilotFailureTolerance: {},
	MetricQuotaCPUAllocated:         {"quota"},
	MetricQuotaCPUUsed:              {"quota"},
	MetricQuotaMemoryAllocated:      {"quota"},
	MetricQuotaMemoryUsed:           {"quota"},
}

// GaugeMetrics are the gauge metrics, sorted.
var GaugeMetrics = slices.Sorted(maps.Keys(gaugeLabels))

// GaugeLabels returns the labels of a gauge metric, false if it is none.
func GaugeLabels(metric string) ([]string, bool) {
	labels, ok := gaugeLabels[metric]
	return labels, ok
}

// Gauge is one sampled value of a gauge metric.
type Gauge struct {
	Metric string
	Labels map[string]string
	Value  float64
}

// GaugeSeries identifies a series of a gauge metric.
type GaugeSeries struct {
	Metric string
	Labels map[string]string
}

// key is the identity of a gauge series: the metric and its labels in the
// order of gaugeLabels.
func gaugeKey(metric string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(metric)
	for _, name := range gaugeLabels[metric] {
		b.WriteString("\x00")
		b.WriteString(labels[name])
	}
	return b.String()
}

// gaugeTSLabels are the tstorage labels of a gauge series.
func gaugeTSLabels(metric string, labels map[string]string) []tstorage.Label {
	out := make([]tstorage.Label, 0, len(gaugeLabels[metric]))
	for _, name := range gaugeLabels[metric] {
		out = append(out, tstorage.Label{Name: name, Value: labels[name]})
	}
	return out
}

// RecordGauges stores values sampled by the server at the time at: the last
// value of an interval counts. Unknown metrics are ignored; new series
// beyond max_series are left out.
func (s *Store) RecordGauges(at time.Time, gauges []Gauge) {
	interval := int64(s.cfg.SampleInterval / time.Second)
	ts := at.Unix()
	start := ts - ts%interval

	s.mu.Lock()
	defer s.mu.Unlock()
	if start+interval <= s.written {
		return
	}
	s.newest = max(s.newest, ts)
	pending := s.gaugePending[start]
	if pending == nil {
		pending = map[string]float64{}
		s.gaugePending[start] = pending
	}
	for _, g := range gauges {
		if _, ok := gaugeLabels[g.Metric]; !ok {
			continue
		}
		key := gaugeKey(g.Metric, g.Labels)
		if _, ok := s.gauges[key]; !ok {
			if s.cfg.MaxSeries > 0 && s.seriesCountLocked()+2 > s.cfg.MaxSeries {
				s.droppedGroups.Add(1)
				continue
			}
			labels := map[string]string{}
			for _, name := range gaugeLabels[g.Metric] {
				labels[name] = g.Labels[name]
			}
			s.gauges[key] = GaugeSeries{Metric: g.Metric, Labels: labels}
			s.seriesDirty = true
		}
		pending[key] = g.Value
	}
}

// gaugeRowsLocked returns the rows of the gauge windows that ended by limit
// and drops them; s.mu is held.
func (s *Store) gaugeRowsLocked(limit, interval int64) []tstorage.Row {
	var rows []tstorage.Row
	for start, values := range s.gaugePending {
		if start+interval > limit {
			continue
		}
		for key, v := range values {
			series, ok := s.gauges[key]
			if !ok {
				continue
			}
			labels := gaugeTSLabels(series.Metric, series.Labels)
			rows = append(rows,
				tstorage.Row{Metric: series.Metric + ".avg", Labels: labels, DataPoint: tstorage.DataPoint{Timestamp: start, Value: v}},
				tstorage.Row{Metric: series.Metric + ".max", Labels: labels, DataPoint: tstorage.DataPoint{Timestamp: start, Value: v}})
		}
		delete(s.gaugePending, start)
	}
	return rows
}

// Gauges returns the gauge series, sorted by metric and labels.
func (s *Store) Gauges() []GaugeSeries {
	s.mu.Lock()
	keys := slices.Sorted(maps.Keys(s.gauges))
	out := make([]GaugeSeries, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.gauges[k])
	}
	s.mu.Unlock()
	return out
}

// QueryGauge returns the points of a gauge series like Query.
func (s *Store) QueryGauge(metric string, labels map[string]string, from, to time.Time, maxPoints int) ([]Point, time.Duration, error) {
	if _, ok := gaugeLabels[metric]; !ok {
		return nil, 0, errUnknownMetric(metric)
	}
	return s.query(metric, gaugeTSLabels(metric, labels), false, from, to, maxPoints)
}
