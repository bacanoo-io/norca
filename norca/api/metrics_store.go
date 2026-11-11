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
	// MetricsGroupAllocsStarting, MetricsGroupAllocsFailed and
	// MetricsGroupAllocsLost are the starting, the failed and the lost
	// allocations of the group.
	MetricsGroupAllocsStarting = "norca.group.allocs_starting"
	MetricsGroupAllocsFailed   = "norca.group.allocs_failed"
	MetricsGroupAllocsLost     = "norca.group.allocs_lost"
	// MetricsGroupRestarts is the number of task restarts; Avg is the sum in
	// the step.
	MetricsGroupRestarts = "norca.group.restarts"
	// MetricsGroupMemorySwapMB is the swap in use in MiB, where the driver
	// measures it.
	MetricsGroupMemorySwapMB = "norca.group.memory_swap_mb"
)

// The host statistics of a client node (labels node, dc, pool and class),
// and the nodes of a datacenter and node pool by state (labels dc and pool).
const (
	MetricsNodeCPUUsed           = "norca.node.cpu_used"
	MetricsNodeCPUMHz            = "norca.node.cpu_mhz"
	MetricsNodeMemoryUsedMB      = "norca.node.memory_used_mb"
	MetricsNodeMemoryTotalMB     = "norca.node.memory_total_mb"
	MetricsNodeDiskUsedMB        = "norca.node.disk_used_mb"
	MetricsNodeDiskTotalMB       = "norca.node.disk_total_mb"
	MetricsNodeInodesUsedPercent = "norca.node.inodes_used_percent"

	// A CSI volume on a node (labels ns, volume, plugin, node), as its node
	// plugin reports it; abnormal is 1 while the plugin reports the volume
	// abnormal.
	MetricsVolumeUsedBytes      = "norca.volume.used_bytes"
	MetricsVolumeAvailableBytes = "norca.volume.available_bytes"
	MetricsVolumeCapacityBytes  = "norca.volume.capacity_bytes"
	MetricsVolumeInodesUsed     = "norca.volume.inodes_used"
	MetricsVolumeInodesFree     = "norca.volume.inodes_free"
	MetricsVolumeAbnormal       = "norca.volume.abnormal"

	// The capacity of the storage of a CSI plugin (labels plugin, topology),
	// as a controller with GET_CAPACITY reports it to the leader.
	MetricsCSICapacityAvailable = "norca.csi.capacity_available_bytes"
	MetricsCSIMaximumVolumeSize = "norca.csi.maximum_volume_size_bytes"
	MetricsCSIMinimumVolumeSize = "norca.csi.minimum_volume_size_bytes"

	MetricsNodesDown         = "norca.nodes.down"
	MetricsNodesDisconnected = "norca.nodes.disconnected"
	MetricsNodesDraining     = "norca.nodes.draining"
	MetricsNodesIneligible   = "norca.nodes.ineligible"
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

	MetricsClusterBlockedCPU             = "norca.cluster.blocked_cpu_mhz"
	MetricsClusterBlockedMemory          = "norca.cluster.blocked_memory_mb"
	MetricsClusterBlockedEvals           = "norca.cluster.blocked_evals"
	MetricsClusterBlockedAllocsExclusive = "norca.cluster.blocked_allocs_exclusive"
	MetricsClusterBlockedUnverifiable    = "norca.cluster.blocked_unverifiable"

	MetricsAutopilotHealthy          = "norca.cluster.autopilot_healthy"
	MetricsAutopilotFailureTolerance = "norca.cluster.autopilot_failure_tolerance"

	MetricsAutopilotVoters            = "norca.cluster.autopilot_voters"
	MetricsAutopilotStandbysReady     = "norca.cluster.autopilot_standbys_ready"
	MetricsAutopilotStandbyPromotions = "norca.cluster.autopilot_standby_promotions"
	MetricsAutopilotStandbyDemotions  = "norca.cluster.autopilot_standby_demotions"

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

	// Labels select the series of a gauge metric (dc, pool and class, or
	// quota); the job and the group are ignored then. A cluster gauge
	// without a class is the series of all the nodes of the datacenter and
	// pool.
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

	// Retention is the retention in effect on the server that answers for
	// the store that keeps the metric (the hosts store for norca.node.*,
	// norca.server.* and norca.bus.*): the longest range it holds.
	Retention time.Duration

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

	// Retention is the retention in effect on the server that answers,
	// HostRetention that of its hosts store (norca.node.*, norca.server.*
	// and norca.bus.*).
	Retention     time.Duration
	HostRetention time.Duration

	Groups []*MetricsGroup
	Gauges []*MetricsGaugeSeries
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

// MetricsStoreStatus is the state of the metrics store of a server: the
// main store, and the hosts store in Hosts.
type MetricsStoreStatus struct {
	// Enabled is false when the server keeps no history.
	Enabled bool
	Server  string

	Partitions     int
	DiskPartitions int
	DiskBytes      int64
	MaxDiskSize    int64

	// Series is the number of the series of both stores, which MaxSeries
	// limits.
	Series    int
	MaxSeries int

	// Oldest and Newest are the times of the oldest and the newest point.
	Oldest time.Time
	Newest time.Time

	// DroppedReports counts the reports too old to store, DroppedGroups the
	// task groups left out at the series limit.
	DroppedReports uint64

	// DuplicateReports counts the reports the bus delivered again.
	DuplicateReports uint64
	DroppedGroups    uint64

	// ForgottenSeries counts the series the store forgot for having no point
	// within the retention, as they counted toward MaxSeries.
	ForgottenSeries uint64

	// Retention is the retention in effect; a reload of the server
	// configuration changes it, MaxDiskSize and MaxSeries.
	Retention      time.Duration
	SampleInterval time.Duration

	// Hosts is the state of the hosts store, which keeps the host metrics
	// (norca.node.*, norca.server.* and norca.bus.*) with their own
	// retention and disk cap; nil when the store is disabled.
	Hosts *MetricsStoreDatabaseStatus
}

// MetricsStoreDatabaseStatus is the state of one database of the metrics
// store of a server.
type MetricsStoreDatabaseStatus struct {
	Partitions     int
	DiskPartitions int
	DiskBytes      int64
	MaxDiskSize    int64

	// Series is the number of the series of the database.
	Series int

	// Oldest and Newest are the times of the oldest and the newest point.
	Oldest time.Time
	Newest time.Time

	// Retention is the retention in effect; a reload of the server
	// configuration changes it and MaxDiskSize.
	Retention time.Duration
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
