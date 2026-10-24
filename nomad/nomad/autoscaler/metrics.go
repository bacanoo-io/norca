// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"errors"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Names of the built-in metrics. They are computed by the leader from the
// task group usage the clients send with Node.UpdateAlloc and can be used in
// a check without a query.
const (
	// MetricCPU is the CPU usage as a percentage of the allocated CPU.
	MetricCPU = "nomad.cpu"

	// MetricMemory is the memory usage as a percentage of the allocated
	// memory.
	MetricMemory = "nomad.memory"

	// MetricCPUMHz is the CPU usage in MHz.
	MetricCPUMHz = "nomad.cpu_mhz"

	// MetricMemoryMB is the memory usage in MiB.
	MetricMemoryMB = "nomad.memory_mb"
)

// usageMetric indexes the values kept per bucket.
type usageMetric int

const (
	usageCPUPercent usageMetric = iota
	usageMemoryPercent
	usageCPUMHz
	usageMemoryMB
	numUsageMetrics
)

// builtinMetrics maps the built-in metric names to the collected values.
var builtinMetrics = map[string]usageMetric{
	MetricCPU:      usageCPUPercent,
	MetricMemory:   usageMemoryPercent,
	MetricCPUMHz:   usageCPUMHz,
	MetricMemoryMB: usageMemoryMB,
}

// IsBuiltinMetric reports whether name is one of the built-in metrics.
func IsBuiltinMetric(name string) bool {
	_, ok := builtinMetrics[name]
	return ok
}

const (
	// usageBuckets is the number of time buckets of a window. The memory of
	// a task group is fixed: it does not depend on the number of nodes or
	// allocations reporting.
	usageBuckets = 30

	// maxUsageGroups bounds the number of task groups the collector keeps,
	// to protect the leader from misbehaving clients.
	maxUsageGroups = 100_000
)

var (
	// ErrInsufficientData is returned for the built-in metrics until one
	// full metric window has been collected since the autoscaler started on
	// this leader. Until then the checks using them do not scale.
	ErrInsufficientData = errors.New("insufficient data: the metric window since the leader change is not complete yet")

	// errNoUsage is returned when no client reported usage for the task
	// group within the queried range.
	errNoUsage = errors.New("no usage reported for the task group")
)

// usageBucket aggregates the reports received in one time bucket.
type usageBucket struct {
	// start is the start of the bucket in Unix nanoseconds; 0 if unused.
	start int64

	// weight is the number of allocation samples in the bucket.
	weight float64

	// sum is the weighted sum of the averages, max the largest maximum.
	sum [numUsageMetrics]float64
	max [numUsageMetrics]float64
}

// groupUsage is the usage window of one task group, a ring of buckets.
type groupUsage struct {
	buckets [usageBuckets]usageBucket

	// last is when the latest report of the group was received.
	last time.Time
}

// usageAggregate is the result of a collector query.
type usageAggregate struct {
	avg float64
	max float64
}

// usageCollector keeps the task group usage reported by clients in memory,
// for a sliding window. It runs on the leader only: it is reset on every
// leadership change and reports ErrInsufficientData until one full window
// has passed since then. Nothing of it is written to Raft.
type usageCollector struct {
	l sync.Mutex

	active      bool
	window      time.Duration
	bucketWidth time.Duration
	started     time.Time
	lastSweep   time.Time
	groups      map[targetKey]*groupUsage
}

func newUsageCollector() *usageCollector {
	return &usageCollector{groups: map[targetKey]*groupUsage{}}
}

// setActive starts or stops collecting. Any change, including a different
// window, drops all data and restarts the insufficient data period.
func (c *usageCollector) setActive(active bool, window time.Duration, now time.Time) {
	if window <= 0 {
		window = DefaultMetricsWindow
	}

	c.l.Lock()
	defer c.l.Unlock()

	if c.active == active && (!active || c.window == window) {
		return
	}

	c.active = active
	c.window = window
	c.bucketWidth = window / usageBuckets
	if c.bucketWidth <= 0 {
		c.bucketWidth = 1
	}
	c.started = now
	c.lastSweep = now
	c.groups = map[targetKey]*groupUsage{}
	metrics.SetGauge([]string{"nomad", "autoscaler", "usage", "groups"}, 0)
}

// record adds a client report received at now.
func (c *usageCollector) record(now time.Time, report *structs.TaskGroupUsageReport) {
	if report == nil {
		return
	}

	c.l.Lock()
	defer c.l.Unlock()

	if !c.active {
		return
	}

	nowNano := now.UnixNano()
	width := int64(c.bucketWidth)
	start := nowNano - nowNano%width
	slot := (nowNano / width) % usageBuckets

	for _, g := range report.Groups {
		if g == nil || g.Samples <= 0 || g.JobID == "" || g.TaskGroup == "" {
			continue
		}
		ns := g.Namespace
		if ns == "" {
			ns = structs.DefaultNamespace
		}
		key := targetKey{namespace: ns, job: g.JobID, group: g.TaskGroup}

		gu, ok := c.groups[key]
		if !ok {
			if len(c.groups) >= maxUsageGroups {
				metrics.IncrCounter([]string{"nomad", "autoscaler", "usage", "dropped"}, 1)
				continue
			}
			gu = &groupUsage{}
			c.groups[key] = gu
		}
		gu.last = now

		b := &gu.buckets[slot]
		if b.start != start {
			*b = usageBucket{start: start}
		}

		w := float64(g.Samples)
		values := [numUsageMetrics]structs.TaskGroupUsageValue{
			usageCPUPercent:    g.CPUPercent,
			usageMemoryPercent: g.MemoryPercent,
			usageCPUMHz:        g.CPUMHz,
			usageMemoryMB:      g.MemoryMB,
		}
		b.weight += w
		for i, v := range values {
			b.sum[i] += v.Avg * w
			if v.Max > b.max[i] {
				b.max[i] = v.Max
			}
		}
	}

	if now.Sub(c.lastSweep) >= c.window {
		c.sweepLocked(now)
	}
}

// sweepLocked drops the task groups without a report within the window.
func (c *usageCollector) sweepLocked(now time.Time) {
	c.lastSweep = now
	for key, gu := range c.groups {
		if now.Sub(gu.last) > c.window {
			delete(c.groups, key)
		}
	}
	metrics.SetGauge([]string{"nomad", "autoscaler", "usage", "groups"}, float32(len(c.groups)))
}

// query aggregates the usage of a task group received in [from, to],
// limited to the collector's window.
func (c *usageCollector) query(now time.Time, key targetKey, metric usageMetric, from, to time.Time) (usageAggregate, error) {
	c.l.Lock()
	defer c.l.Unlock()

	if !c.active || now.Sub(c.started) < c.window {
		return usageAggregate{}, ErrInsufficientData
	}

	gu, ok := c.groups[key]
	if !ok {
		return usageAggregate{}, errNoUsage
	}

	// An instant query uses the latest bucket.
	if !from.Before(to) {
		from = to.Add(-c.bucketWidth)
	}
	if oldest := now.Add(-c.window); from.Before(oldest) {
		from = oldest
	}
	fromNano, toNano := from.UnixNano(), to.UnixNano()
	width := int64(c.bucketWidth)

	var weight, sum, maxValue float64
	for i := range gu.buckets {
		b := &gu.buckets[i]
		if b.start == 0 || b.weight == 0 || b.start+width <= fromNano || b.start > toNano {
			continue
		}
		weight += b.weight
		sum += b.sum[metric]
		if b.max[metric] > maxValue {
			maxValue = b.max[metric]
		}
	}
	if weight == 0 {
		return usageAggregate{}, errNoUsage
	}
	return usageAggregate{avg: sum / weight, max: maxValue}, nil
}

// numGroups returns the number of task groups kept.
func (c *usageCollector) numGroups() int {
	c.l.Lock()
	defer c.l.Unlock()
	return len(c.groups)
}

// RecordUsage hands a task group usage report received with
// Node.UpdateAlloc to the collector. Reports are ignored while the
// autoscaler does not run on this server.
func (a *Autoscaler) RecordUsage(report *structs.TaskGroupUsageReport) {
	a.usage.record(a.now(), report)
}

// UsageGroups returns the number of task groups with usage data.
func (a *Autoscaler) UsageGroups() int {
	return a.usage.numGroups()
}

// UsageInterval returns the aggregation window the clients should use for
// their task group usage reports, or zero if the built-in autoscaler is
// disabled and clients should not send them.
func (a *Autoscaler) UsageInterval() time.Duration {
	a.l.Lock()
	defer a.l.Unlock()
	if !a.config.Enabled {
		return 0
	}
	if a.config.ClientMetricsInterval <= 0 {
		return DefaultClientMetricsInterval
	}
	return a.config.ClientMetricsInterval
}
