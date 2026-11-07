// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// TestStore_NodeHost checks the usage of the nodes: the measurements of the
// reports of one interval averaged by their samples, the largest as the
// maximum, the totals, the values the client did not measure, and a report
// late for a written interval.
func TestStore_NodeHost(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	base := time.Now().Add(-time.Hour).Truncate(30 * time.Second)
	labels := map[string]string{"node": "n1", "dc": "dc1", "pool": "default", "class": "big"}
	host := func(samples int, cpu, cpuMax float64) *structs.NodeHostUsage {
		return &structs.NodeHostUsage{Samples: samples,
			CPUPercent: structs.TaskGroupUsageValue{Avg: cpu, Max: cpuMax},
			CPUMHz:     structs.TaskGroupUsageValue{Avg: cpu * 40, Max: cpuMax * 40}, CPUTotalMHz: 4000,
			MemoryUsedMB: structs.TaskGroupUsageValue{Avg: 1000, Max: 1500}, MemoryTotalMB: 8192,
			DiskUsedMB: structs.TaskGroupUsageValue{Avg: 500, Max: 600}, DiskTotalMB: 10240,
			InodesUsedPercent: structs.TaskGroupUsageValue{Avg: 3, Max: 4},
		}
	}
	s.RecordNodeHost(labels, host(10, 10, 20), base.Add(time.Second))
	s.RecordNodeHost(labels, host(30, 50, 90), base.Add(11*time.Second))
	// Without the CPU of the node, the disk and the memory: only cpu_used.
	other := map[string]string{"node": "n2", "dc": "dc1", "pool": "default", "class": ""}
	s.RecordNodeHost(other, &structs.NodeHostUsage{Samples: 1, CPUPercent: structs.TaskGroupUsageValue{Avg: 5, Max: 5}}, base.Add(time.Second))
	// Ignored: no samples, no node.
	s.RecordNodeHost(other, &structs.NodeHostUsage{}, base.Add(time.Second))
	s.RecordNodeHost(map[string]string{"dc": "dc1"}, host(1, 1, 1), base.Add(time.Second))
	must.NoError(t, s.flush(base.Add(30*time.Second)))

	query := func(metric string, labels map[string]string) []Point {
		points, _, err := s.QueryGauge(metric, labels, base, base.Add(time.Minute), 0)
		must.NoError(t, err)
		return points
	}
	cpu := query(MetricNodeCPUUsed, labels)
	must.Len(t, 1, cpu)
	must.Eq(t, 40.0, cpu[0].Avg) // (10*10 + 50*30) / 40
	must.Eq(t, 90.0, cpu[0].Max)
	must.Eq(t, 1600.0, query(MetricNodeCPUMHz, labels)[0].Avg)
	must.Eq(t, 1500.0, query(MetricNodeMemoryUsedMB, labels)[0].Max)
	must.Eq(t, 8192.0, query(MetricNodeMemoryTotalMB, labels)[0].Avg)
	must.Eq(t, 600.0, query(MetricNodeDiskUsedMB, labels)[0].Max)
	must.Eq(t, 10240.0, query(MetricNodeDiskTotalMB, labels)[0].Max)
	must.Eq(t, 3.0, query(MetricNodeInodesUsedPercent, labels)[0].Avg)
	must.Eq(t, 5.0, query(MetricNodeCPUUsed, other)[0].Avg)
	must.SliceEmpty(t, query(MetricNodeMemoryUsedMB, other))
	must.SliceEmpty(t, query(MetricNodeCPUMHz, other))
	must.Len(t, 8, s.Gauges()) // 7 of n1, 1 of n2

	// Late for the written interval.
	s.RecordNodeHost(labels, host(1, 99, 99), base.Add(20*time.Second))
	must.NoError(t, s.flush(base.Add(time.Minute)))
	cpu = query(MetricNodeCPUUsed, labels)
	must.Len(t, 1, cpu)
	must.Eq(t, 40.0, cpu[0].Avg)
}

// TestStore_WorkloadState checks the allocations by status and the task
// restarts the server samples, and the peak memory of the reports, which
// only the measurements that have it make.
func TestStore_WorkloadState(t *testing.T) {
	ci.Parallel(t)
	s := openTest(t, testConfig(t.TempDir()))
	defer func() { must.NoError(t, s.Close()) }()

	base := time.Now().Add(-time.Hour).Truncate(time.Minute)
	failedOnly := GroupKey{Namespace: "default", Job: "old", Group: "web"}
	restarting := GroupKey{Namespace: "default", Job: "loop", Group: "web"}
	s.RecordAllocs(base.Add(time.Second), map[GroupKey]AllocCounts{
		web:        {Running: 1, Starting: 2, Failed: 3, Lost: 4, Restarts: 2},
		failedOnly: {Failed: 5},
		restarting: {Restarts: 1},
	})
	// Two samples in one interval: the restarts add up, the states are the
	// last.
	s.RecordAllocs(base.Add(2*time.Second), map[GroupKey]AllocCounts{web: {Running: 1, Starting: 1, Restarts: 3}})
	s.RecordAllocs(base.Add(31*time.Second), map[GroupKey]AllocCounts{web: {Running: 1}})

	peak := func(peakSamples int, avg, mx float64) *structs.TaskGroupUsageReport {
		r := usage("api", 4, 10, 10)
		r.Groups[0].MemoryPeakMB = structs.TaskGroupUsageValue{Avg: avg, Max: mx}
		r.Groups[0].MemoryPeakSamples = peakSamples
		return r
	}
	s.Ingest(peak(1, 100, 100), base.Add(3*time.Second))
	s.Ingest(peak(3, 300, 400), base.Add(4*time.Second))
	s.Ingest(peak(0, 0, 0), base.Add(32*time.Second)) // not measured
	must.NoError(t, s.flush(base.Add(time.Minute)))

	query := func(key GroupKey, metric string, maxPoints int) []Point {
		points, _, err := s.Query(key, metric, base, base.Add(time.Minute), maxPoints)
		must.NoError(t, err)
		return points
	}
	restarts := query(web, MetricRestarts, 0)
	must.Len(t, 2, restarts)
	must.Eq(t, 5.0, restarts[0].Avg)
	must.Eq(t, 3.0, restarts[0].Max)
	must.Eq(t, 0.0, restarts[1].Avg)
	must.Eq(t, 5.0, query(web, MetricRestarts, 1)[0].Avg) // a step sums
	must.Eq(t, 1.0, query(web, MetricAllocsStarting, 0)[0].Avg)
	must.Eq(t, 0.0, query(web, MetricAllocsFailed, 0)[0].Avg)
	must.Eq(t, 0.0, query(web, MetricAllocsLost, 0)[0].Avg)
	must.Eq(t, 1.0, query(restarting, MetricRestarts, 0)[0].Avg)

	memPeak := query(web, MetricMemoryPeakMB, 0)
	must.Len(t, 1, memPeak)
	must.Eq(t, 250.0, memPeak[0].Avg) // (100*1 + 300*3) / 4
	must.Eq(t, 400.0, memPeak[0].Max)
	must.Len(t, 2, query(web, MetricMemoryMB, 0))

	// Failed allocations alone make no series: old jobs keep them.
	must.Eq(t, []GroupKey{web, restarting}, s.Groups())
}
