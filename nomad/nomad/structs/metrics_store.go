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

	// MetricsStoreStatusRPCMethod returns the state of the metrics store of
	// a server.
	//
	// Args: MetricsStoreStatusRequest
	// Reply: MetricsStoreStatusResponse
	MetricsStoreStatusRPCMethod = "Operator.MetricsStoreStatus"
)

// MetricsHistoryRequest asks for the history of a metric of a task group in
// the namespace of the request. Every server stores every report, so the
// server that receives the request answers; ServerID asks another server of
// the region instead (one that was down has a gap).
type MetricsHistoryRequest struct {
	JobID  string
	Group  string
	Metric string

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

// MetricsStoreStatus is the state of the metrics store of one server.
type MetricsStoreStatus struct {
	// Enabled is false when the server keeps no history; the other fields
	// but Server are empty then.
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
