// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import "time"

const (
	// MetricsStoreHistoryRPCMethod returns the history of a metric of a task
	// group from the metrics store of a server.
	//
	// Args: MetricsHistoryRequest
	// Reply: MetricsHistoryResponse
	MetricsStoreHistoryRPCMethod = "MetricsStore.History"

	// MetricsStoreSeriesRPCMethod lists the task groups and the gauge
	// series of the metrics store of a server the token may read.
	//
	// Args: MetricsSeriesRequest
	// Reply: MetricsSeriesResponse
	MetricsStoreSeriesRPCMethod = "MetricsStore.Series"

	// MetricsStoreSummaryRPCMethod returns the usage of a task group over a
	// window against its allocated resources (right-sizing).
	//
	// Args: MetricsSummaryRequest
	// Reply: MetricsSummaryResponse
	MetricsStoreSummaryRPCMethod = "MetricsStore.Summary"

	// MetricsStoreStatusRPCMethod returns the state of the metrics store of
	// a server.
	//
	// Args: MetricsStoreStatusRequest
	// Reply: MetricsStoreStatusResponse
	MetricsStoreStatusRPCMethod = "Operator.MetricsStoreStatus"
)

// MetricsHistoryRequest asks for the history of a metric. A metric of a
// task group (norca.group.*) needs JobID and Group, in the namespace of the
// request; a gauge metric (norca.cluster.*, norca.quota.*) is selected by
// Labels (dc and pool, or quota). Every server stores every report, so the
// server that receives the request answers; ServerID asks another server of
// the region instead (one that was down has a gap).
type MetricsHistoryRequest struct {
	JobID  string
	Group  string
	Metric string
	Labels map[string]string

	// From and To bound the history (To exclusive); zero To is now, zero
	// From an hour before To.
	From time.Time
	To   time.Time

	// MaxPoints is the largest number of points; 0 is the default (360).
	MaxPoints int

	ServerID string
	QueryOptions
}

// MetricsHistoryResponse is the history of a metric. Enabled is false when
// the server keeps no history; the points are empty then.
type MetricsHistoryResponse struct {
	Enabled bool
	Metric  string

	// Retention is the retention in effect on the server that answers for
	// the store that keeps the metric (the hosts store for norca.node.*,
	// norca.server.* and norca.bus.*): the longest range it holds.
	Retention time.Duration

	// Step is the time range of one point.
	Step   time.Duration
	Points []*MetricsPoint
	QueryMeta
}

// MetricsPoint is one step of a history: the mean of the averages and the
// maximum of the maxima of the stored points in the step. Time is the start
// of the step.
type MetricsPoint struct {
	Time time.Time
	Avg  float64
	Max  float64
}

// MetricsSeriesRequest lists the series of the metrics store of a server.
type MetricsSeriesRequest struct {
	ServerID string
	QueryOptions
}

// MetricsSeriesResponse lists the task groups with series in the namespaces
// the token may read jobs of, and the gauge series it may read.
type MetricsSeriesResponse struct {
	Enabled bool

	// Retention is the retention in effect on the server that answers,
	// HostRetention that of its hosts store (norca.node.*, norca.server.*
	// and norca.bus.*).
	Retention     time.Duration
	HostRetention time.Duration

	Groups []*MetricsGroup
	Gauges []*MetricsGaugeSeries
	QueryMeta
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

// MetricsSummaryRequest asks for the usage of a task group in the namespace
// of the request over Window (default 720h) before now.
type MetricsSummaryRequest struct {
	JobID    string
	Group    string
	Window   time.Duration
	ServerID string
	QueryOptions
}

// MetricsSummaryResponse is the usage of a task group over a window, per
// allocation, against the resources the current job version allocates.
type MetricsSummaryResponse struct {
	Enabled bool
	Window  time.Duration

	// Allocated is the CPU (MHz) and memory (MiB) of an allocation of the
	// group, the sum of its tasks; zero when the job is gone.
	AllocatedCPUMHz   int64
	AllocatedMemoryMB int64

	// Metrics is the summary of each usage metric of the group, by name.
	Metrics map[string]*MetricsSummary
	QueryMeta
}

// MetricsSummary is the usage of a metric over a window: the mean of the
// interval averages, the 95th percentile and the largest of the interval
// maxima, and the number of intervals they are computed from.
type MetricsSummary struct {
	Avg     float64
	P95     float64
	Max     float64
	Samples int
}

// MetricsStoreStatusRequest asks a server for the state of its metrics
// store; ServerID asks another server of the region.
type MetricsStoreStatusRequest struct {
	ServerID string
	QueryOptions
}

// MetricsStoreStatusResponse is the state of the metrics store of a server.
type MetricsStoreStatusResponse struct {
	Status *MetricsStoreStatus
	QueryMeta
}

// MetricsStoreStatus is the state of the metrics store of one server: the
// main store, and the hosts store in Hosts.
type MetricsStoreStatus struct {
	// Enabled is false when the server keeps no history; the other fields
	// but Server are empty then.
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
