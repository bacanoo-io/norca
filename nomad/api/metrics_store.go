// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"net/url"
	"strconv"
	"time"
)

// The metrics of a task group in the metrics store, per allocation.
const (
	// MetricsGroupCPU is the CPU usage in percent of the allocated CPU.
	MetricsGroupCPU = "norca.group.cpu"
	// MetricsGroupCPUMHz is the CPU usage in MHz.
	MetricsGroupCPUMHz = "norca.group.cpu_mhz"
	// MetricsGroupMemory is the memory usage in percent of the allocated
	// memory.
	MetricsGroupMemory = "norca.group.memory"
	// MetricsGroupMemoryMB is the memory usage in MiB.
	MetricsGroupMemoryMB = "norca.group.memory_mb"
	// MetricsGroupCPUThrottled is the time the tasks were throttled by
	// their CPU limit, in percent.
	MetricsGroupCPUThrottled = "norca.group.cpu_throttled"
	// MetricsGroupMemoryCacheMB is the page cache in MiB.
	MetricsGroupMemoryCacheMB = "norca.group.memory_cache_mb"
	// MetricsGroupOOMKills is the number of tasks killed for running out of
	// memory; Avg is the sum in the step.
	MetricsGroupOOMKills = "norca.group.oom_kills"
	// MetricsGroupAllocsRunning and MetricsGroupAllocsQueued are the running
	// and the queued allocations of the group.
	MetricsGroupAllocsRunning = "norca.group.allocs_running"
	MetricsGroupAllocsQueued  = "norca.group.allocs_queued"
)

// The gauge metrics the servers sample. The cluster metrics have the labels
// dc and pool, the quota metrics the label quota.
const (
	MetricsClusterCPUAllocated    = "norca.cluster.cpu_allocated"
	MetricsClusterCPUUsed         = "norca.cluster.cpu_used"
	MetricsClusterMemoryAllocated = "norca.cluster.memory_allocated"
	MetricsClusterMemoryUsed      = "norca.cluster.memory_used"
	MetricsClusterDiskAllocated   = "norca.cluster.disk_allocated"
	MetricsClusterNodesReady      = "norca.cluster.nodes_ready"

	MetricsAutopilotHealthy          = "norca.cluster.autopilot_healthy"
	MetricsAutopilotFailureTolerance = "norca.cluster.autopilot_failure_tolerance"

	MetricsQuotaCPUAllocated    = "norca.quota.cpu_mhz_allocated"
	MetricsQuotaCPUUsed         = "norca.quota.cpu_mhz_used"
	MetricsQuotaMemoryAllocated = "norca.quota.memory_mb_allocated"
	MetricsQuotaMemoryUsed      = "norca.quota.memory_mb_used"
)

// MetricsStore reads the history of the task group usage the servers keep.
type MetricsStore struct {
	c *Client
}

// MetricsStore returns a handle on the metrics store.
func (c *Client) MetricsStore() *MetricsStore {
	return &MetricsStore{c: c}
}

// MetricsHistoryOptions select the range and the resolution of a history.
type MetricsHistoryOptions struct {
	// Metric is one of the MetricsGroup* metrics; empty is MetricsGroupCPU.
	Metric string

	// Labels select the series of a gauge metric (dc and pool, or quota);
	// the job and the group are ignored then.
	Labels map[string]string

	// From and To bound the history (To exclusive); zero To is now, zero
	// From an hour before To.
	From time.Time
	To   time.Time

	// MaxPoints is the largest number of points; 0 is the server default.
	MaxPoints int

	// ServerID asks a given server of the region instead of the one that
	// answers.
	ServerID string
}

// MetricsHistory is the history of a metric of a task group. Enabled is
// false when the server keeps no history.
type MetricsHistory struct {
	Enabled bool
	Metric  string

	// Step is the time range of one point.
	Step   time.Duration
	Points []*MetricsPoint
}

// MetricsPoint is one step of a history: the mean of the averages and the
// maximum of the maxima in the step, which starts at Time.
type MetricsPoint struct {
	Time time.Time
	Avg  float64
	Max  float64
}

// History returns the history of a metric of a task group in the namespace
// of the query options.
func (m *MetricsStore) History(jobID, group string, opts *MetricsHistoryOptions, q *QueryOptions) (*MetricsHistory, *QueryMeta, error) {
	v := url.Values{}
	v.Set("job", jobID)
	v.Set("group", group)
	if opts != nil {
		if opts.Metric != "" {
			v.Set("metric", opts.Metric)
		}
		for name, value := range opts.Labels {
			v.Set(name, value)
		}
		if !opts.From.IsZero() {
			v.Set("from", opts.From.Format(time.RFC3339))
		}
		if !opts.To.IsZero() {
			v.Set("to", opts.To.Format(time.RFC3339))
		}
		if opts.MaxPoints > 0 {
			v.Set("max_points", strconv.Itoa(opts.MaxPoints))
		}
		if opts.ServerID != "" {
			v.Set("server_id", opts.ServerID)
		}
	}
	var out MetricsHistory
	qm, err := m.c.query("/v1/metrics/history?"+v.Encode(), &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, qm, nil
}

// MetricsSeries lists the series of the metrics store the token may read.
type MetricsSeries struct {
	Enabled bool
	Groups  []*MetricsGroup
	Gauges  []*MetricsGaugeSeries
}

// MetricsGroup is a task group with series.
type MetricsGroup struct {
	Namespace string
	JobID     string
	Group     string
}

// MetricsGaugeSeries is a series of a gauge metric.
type MetricsGaugeSeries struct {
	Metric string
	Labels map[string]string
}

// Series lists the task groups with series in the namespaces the token may
// read jobs of, and the gauge series it may read.
func (m *MetricsStore) Series(q *QueryOptions) (*MetricsSeries, *QueryMeta, error) {
	var out MetricsSeries
	qm, err := m.c.query("/v1/metrics/series", &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, qm, nil
}

// UsageSummary is the usage of a task group over a window, per allocation,
// against the CPU (MHz) and memory (MiB) an allocation of the current job
// version has.
type UsageSummary struct {
	Enabled           bool
	Window            time.Duration
	AllocatedCPUMHz   int64
	AllocatedMemoryMB int64
	// Metrics is the summary of each usage metric, by name.
	Metrics map[string]*UsageSummaryValue
}

// UsageSummaryValue is the mean of the interval averages, the 95th
// percentile and the largest of the interval maxima of a metric, and the
// number of intervals.
type UsageSummaryValue struct {
	Avg     float64
	P95     float64
	Max     float64
	Samples int
}

// Summary returns the usage of a task group in the namespace of the query
// options over window (0: 720h) before now.
func (m *MetricsStore) Summary(jobID, group string, window time.Duration, q *QueryOptions) (*UsageSummary, *QueryMeta, error) {
	v := url.Values{}
	v.Set("job", jobID)
	v.Set("group", group)
	if window > 0 {
		v.Set("window", window.String())
	}
	var out UsageSummary
	qm, err := m.c.query("/v1/metrics/summary?"+v.Encode(), &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, qm, nil
}

// MetricsStoreStatus is the state of the metrics store of a server.
type MetricsStoreStatus struct {
	// Enabled is false when the server keeps no history.
	Enabled bool
	Server  string

	Partitions     int
	DiskPartitions int
	DiskBytes      int64
	MaxDiskSize    int64
	Series         int
	MaxSeries      int

	// Oldest and Newest are the times of the oldest and the newest point.
	Oldest time.Time
	Newest time.Time

	// DroppedReports counts the reports too old to store, DroppedGroups the
	// task groups left out at the series limit.
	DroppedReports uint64
	DroppedGroups  uint64

	Retention      time.Duration
	SampleInterval time.Duration
}

// MetricsStoreStatus returns the state of the metrics store of the server
// that answers the request; serverID (a server name or ID) asks a given
// server of the region instead.
func (op *Operator) MetricsStoreStatus(serverID string, q *QueryOptions) (*MetricsStoreStatus, *QueryMeta, error) {
	path := "/v1/operator/metrics-store"
	if serverID != "" {
		path += "?server_id=" + url.QueryEscape(serverID)
	}
	var out MetricsStoreStatus
	qm, err := op.c.query(path, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, qm, nil
}
