// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

// The metrics of a task group. Each is stored as two series, <name>.avg and
// <name>.max, labeled with the namespace, job and group.
const (
	// MetricCPU is the CPU usage in percent of the allocated CPU, per
	// allocation.
	MetricCPU = "norca.group.cpu"
	// MetricCPUMHz is the CPU usage in MHz, per allocation.
	MetricCPUMHz = "norca.group.cpu_mhz"
	// MetricCPUThrottled is the time the tasks of an allocation were
	// throttled by their CPU limit, in percent.
	MetricCPUThrottled = "norca.group.cpu_throttled"
	// MetricMemory is the memory usage in percent of the allocated memory,
	// per allocation.
	MetricMemory = "norca.group.memory"
	// MetricMemoryMB is the memory usage in MiB, per allocation.
	MetricMemoryMB = "norca.group.memory_mb"
	// MetricMemoryCacheMB is the page cache in MiB, per allocation.
	MetricMemoryCacheMB = "norca.group.memory_cache_mb"
	// MetricOOMKills is the number of tasks of the group killed for running
	// out of memory.
	MetricOOMKills = "norca.group.oom_kills"
	// MetricAllocsRunning and MetricAllocsQueued are the running and the
	// queued allocations of the group, from the state of the servers.
	MetricAllocsRunning = "norca.group.allocs_running"
	MetricAllocsQueued  = "norca.group.allocs_queued"
)

// metricKind is how the values of a metric in one sample interval combine.
type metricKind int

const (
	// kindUsage: the averages of the reports weighted by their samples, the
	// largest maximum.
	kindUsage metricKind = iota
	// kindCounter: the sum of the reports; in a query step the sum of the
	// stored points.
	kindCounter
	// kindState: the last value sampled by the server.
	kindState
)

// metricDef is a metric of a task group.
type metricDef struct {
	name string
	kind metricKind
	// usage returns the value of a report; nil for kindState.
	usage func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue
}

var groupMetricDefs = []metricDef{
	{MetricCPU, kindUsage, func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue { return g.CPUPercent }},
	{MetricCPUMHz, kindUsage, func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue { return g.CPUMHz }},
	{MetricCPUThrottled, kindUsage, func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue { return g.CPUThrottledPercent }},
	{MetricMemory, kindUsage, func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue { return g.MemoryPercent }},
	{MetricMemoryMB, kindUsage, func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue { return g.MemoryMB }},
	{MetricMemoryCacheMB, kindUsage, func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue { return g.MemoryCacheMB }},
	{MetricOOMKills, kindCounter, func(g *structs.TaskGroupUsage) structs.TaskGroupUsageValue {
		v := float64(g.OOMKills)
		return structs.TaskGroupUsageValue{Avg: v, Max: v}
	}},
	{MetricAllocsRunning, kindState, nil},
	{MetricAllocsQueued, kindState, nil},
}

// GroupMetrics are the metrics of a task group.
var GroupMetrics = func() []string {
	out := make([]string, len(groupMetricDefs))
	for i, d := range groupMetricDefs {
		out[i] = d.name
	}
	return out
}()

// seriesPerGroup is the number of series of a task group.
var seriesPerGroup = 2 * len(groupMetricDefs)

// metricIndex returns the index of a metric in groupMetricDefs, -1 if none.
func metricIndex(name string) int {
	for i, d := range groupMetricDefs {
		if d.name == name {
			return i
		}
	}
	return -1
}

// window combines the values of a task group in one sample interval.
type window struct {
	has    [numGroupMetrics]bool
	sum    [numGroupMetrics]float64
	weight [numGroupMetrics]float64
	max    [numGroupMetrics]float64
}

// numGroupMetrics is the number of entries of groupMetricDefs.
const numGroupMetrics = 9

func init() {
	if len(groupMetricDefs) != numGroupMetrics {
		panic("metricsstore: numGroupMetrics differs from groupMetricDefs")
	}
}

// addUsage adds a report of the group.
func (w *window) addUsage(g *structs.TaskGroupUsage) {
	s := float64(g.Samples)
	for i, d := range groupMetricDefs {
		if d.kind == kindState {
			continue
		}
		v := d.usage(g)
		switch d.kind {
		case kindUsage:
			w.sum[i] += v.Avg * s
			w.weight[i] += s
		case kindCounter:
			w.sum[i] += v.Avg
			w.weight[i] = 1
		}
		if !w.has[i] || v.Max > w.max[i] {
			w.max[i] = v.Max
		}
		w.has[i] = true
	}
}

// setState sets a value sampled by the server.
func (w *window) setState(i int, v float64) {
	w.has[i] = true
	w.sum[i], w.weight[i], w.max[i] = v, 1, v
}

// value returns the average (the sum of a counter) and the maximum of a
// metric, false when the window has none.
func (w *window) value(i int) (float64, float64, bool) {
	if !w.has[i] || w.weight[i] == 0 {
		return 0, 0, false
	}
	if groupMetricDefs[i].kind == kindCounter {
		return w.sum[i], w.max[i], true
	}
	return w.sum[i] / w.weight[i], w.max[i], true
}
