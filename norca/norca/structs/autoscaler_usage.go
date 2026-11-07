// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import "time"

// TaskGroupUsageReport is the resource usage of the task groups running on a
// client node, aggregated by the client over one window. Clients publish it
// as JSON on their telemetry subject of the system bus (BusTelemetrySubject)
// when the servers ask for it (see NodeUpdateResponse.TaskGroupUsageInterval),
// split into several reports of the same window when it would exceed
// BusTelemetryMaxMsgSize. Every server reads them from the telemetry stream;
// the leader keeps them in memory for the built-in autoscaler. They are
// never written to Raft.
type TaskGroupUsageReport struct {
	// NodeID is the reporting node.
	NodeID string

	// Window is the length of the client side aggregation window.
	Window time.Duration

	// Groups is the usage of each task group with running allocations on
	// the node.
	Groups []*TaskGroupUsage

	// Host is the usage of the node itself in the window; nil when the
	// client did not measure it. A report split into several messages has
	// it in the first one only.
	Host *NodeHostUsage `json:",omitempty"`
}

// NodeHostUsage is the usage of a client node as a whole (its host
// statistics) over one aggregation window: Avg is the mean of the
// measurements in the window, Max the largest one. The totals are those of
// the newest measurement.
type NodeHostUsage struct {
	// Samples is the number of host measurements in the window.
	Samples int

	// CPUPercent is the CPU time in user and system mode, in percent of the
	// CPU of the node (the mean over its cores).
	CPUPercent TaskGroupUsageValue

	// CPUMHz is the same in MHz, and CPUTotalMHz the CPU of the node in MHz;
	// both zero when the client does not know the CPU of the node.
	CPUMHz      TaskGroupUsageValue
	CPUTotalMHz float64

	// MemoryUsedMB is the memory in use in MiB, MemoryTotalMB the memory of
	// the node.
	MemoryUsedMB  TaskGroupUsageValue
	MemoryTotalMB float64

	// DiskUsedMB is the space in use in MiB on the file system of the
	// allocation directory, DiskTotalMB its size, and InodesUsedPercent its
	// inodes in use in percent.
	DiskUsedMB        TaskGroupUsageValue
	DiskTotalMB       float64
	InodesUsedPercent TaskGroupUsageValue
}

// TaskGroupUsage is the resource usage of the allocations of one task group
// on one client node over one aggregation window. Every value is per
// allocation: Avg is the mean of all allocation measurements in the window,
// Max the largest one.
type TaskGroupUsage struct {
	Namespace string
	JobID     string
	TaskGroup string

	// Allocs is the number of allocations measured in the window.
	Allocs int

	// Samples is the number of allocation measurements the averages are
	// computed from. The server uses it to weight reports.
	Samples int

	// CPUMHz is the CPU usage in MHz.
	CPUMHz TaskGroupUsageValue

	// CPUPercent is the CPU usage as a percentage of the allocated CPU.
	CPUPercent TaskGroupUsageValue

	// MemoryMB is the memory usage in MiB.
	MemoryMB TaskGroupUsageValue

	// MemoryPercent is the memory usage as a percentage of the allocated
	// memory.
	MemoryPercent TaskGroupUsageValue

	// MemoryCacheMB is the page cache in MiB (reclaimable; part of the
	// memory usage when the driver measures the cgroup usage).
	MemoryCacheMB TaskGroupUsageValue

	// MemorySwapMB is the swap in use in MiB, from the MemorySwapSamples
	// measurements that had it: drivers that do not measure the swap
	// (docker on cgroups v2) report none.
	MemorySwapMB      TaskGroupUsageValue
	MemorySwapSamples int

	// CPUThrottledPercent is the time the tasks were throttled by their CPU
	// limit (CFS quota), as a percentage of the time between two
	// measurements; the times of the tasks of an allocation add up.
	CPUThrottledPercent TaskGroupUsageValue

	// OOMKills is the number of tasks killed by the kernel for running out
	// of memory in the window, of all allocations of the group on the node.
	OOMKills int
}

// TaskGroupUsageValue is the average and the maximum of a usage value.
type TaskGroupUsageValue struct {
	Avg float64
	Max float64
}
