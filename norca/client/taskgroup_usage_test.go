// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/client/stats"
	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestTaskGroupUsageReporter_Windows(t *testing.T) {
	ci.Parallel(t)

	r := newTaskGroupUsageReporter()
	start := time.Unix(1_700_000_000, 0)
	alloc := mock.Alloc()
	usage := allocrunnerFakeUsage()

	// Disabled until the servers ask for usage.
	r.agg.Observe(alloc, usage)
	r.windowDone("node", start.Add(time.Hour))
	must.Nil(t, r.take())

	r.setInterval(10*time.Second, start)
	must.Eq(t, 10*time.Second, r.getInterval())

	// Samples are kept until the window has elapsed.
	r.agg.Observe(alloc, usage)
	r.windowDone("node", start.Add(5*time.Second))
	must.Nil(t, r.take())

	r.agg.Observe(alloc, usage)
	r.windowDone("node", start.Add(10*time.Second))
	report := r.take()
	must.NotNil(t, report)
	must.Eq(t, "node", report.NodeID)
	must.Eq(t, 10*time.Second, report.Window)
	must.Len(t, 1, report.Groups)
	must.Eq(t, 2, report.Groups[0].Samples)

	// A report is taken once.
	must.Nil(t, r.take())

	// An empty window produces no report.
	r.windowDone("node", start.Add(20*time.Second))
	must.Nil(t, r.take())

	// Disabling drops pending samples and reports.
	r.agg.Observe(alloc, usage)
	r.setInterval(0, start.Add(25*time.Second))
	r.windowDone("node", start.Add(time.Hour))
	must.Nil(t, r.take())
	must.Nil(t, r.agg.Flush())
}

func allocrunnerFakeUsage() *cstructs.AllocResourceUsage {
	return &cstructs.AllocResourceUsage{
		ResourceUsage: &cstructs.ResourceUsage{
			CpuStats:    &cstructs.CpuStats{TotalTicks: 100},
			MemoryStats: &cstructs.MemoryStats{RSS: 1024 * 1024},
		},
		Timestamp: 1,
	}
}

// TestSplitTaskGroupUsage checks that a report too large for one message of
// the telemetry stream is split into reports of the same window.
func TestSplitTaskGroupUsage(t *testing.T) {
	report := &structs.TaskGroupUsageReport{NodeID: "node", Window: 10 * time.Second}
	for i := 0; i < 100; i++ {
		report.Groups = append(report.Groups, &structs.TaskGroupUsage{
			Namespace: "default", JobID: fmt.Sprintf("job-%03d", i), TaskGroup: "web", Allocs: 1, Samples: 5,
		})
	}

	msgs, err := splitTaskGroupUsage(report, 1<<20)
	must.NoError(t, err)
	must.Len(t, 1, msgs)

	msgs, err = splitTaskGroupUsage(report, 2048)
	must.NoError(t, err)
	must.Greater(t, 1, len(msgs))
	var groups []string
	for _, m := range msgs {
		must.LessEq(t, 2048, len(m))
		var part structs.TaskGroupUsageReport
		must.NoError(t, json.Unmarshal(m, &part))
		must.Eq(t, "node", part.NodeID)
		must.Eq(t, 10*time.Second, part.Window)
		for _, g := range part.Groups {
			groups = append(groups, g.JobID)
		}
	}
	must.Len(t, 100, groups)
	must.Eq(t, "job-099", groups[99])

	msgs, err = splitTaskGroupUsage(&structs.TaskGroupUsageReport{NodeID: "node"}, 2048)
	must.NoError(t, err)
	must.SliceEmpty(t, msgs)

	// The usage of the node is in the first part only, also without groups.
	report.Host = &structs.NodeHostUsage{Samples: 3, MemoryTotalMB: 1024}
	msgs, err = splitTaskGroupUsage(report, 2048)
	must.NoError(t, err)
	must.Greater(t, 1, len(msgs))
	for i, m := range msgs {
		must.LessEq(t, 2048, len(m))
		var part structs.TaskGroupUsageReport
		must.NoError(t, json.Unmarshal(m, &part))
		if i == 0 {
			must.Eq(t, report.Host, part.Host)
		} else {
			must.Nil(t, part.Host)
		}
	}
	msgs, err = splitTaskGroupUsage(&structs.TaskGroupUsageReport{NodeID: "node", Host: report.Host}, 2048)
	must.NoError(t, err)
	must.Len(t, 1, msgs)
}

// hostStats returns a host measurement: two cores, memory and the file
// system of the allocation directory.
func hostStats(ts int64, user, system float64) *stats.HostStats {
	const mib = 1024 * 1024
	return &stats.HostStats{
		Timestamp: ts,
		CPU: []*stats.CPUStats{
			{CPU: "cpu0", User: user, System: system},
			{CPU: "cpu1", User: user / 2, System: system / 2},
		},
		Memory:        &stats.MemoryStats{Total: 8192 * mib, Used: 2048 * mib},
		AllocDirStats: &stats.DiskStats{Size: 10240 * mib, Used: 1024 * mib, InodesUsedPercent: 5},
	}
}

// TestTaskGroupUsageReporter_Host checks the usage of the node in the
// report: the mean and the largest of the measurements of the window, each
// measurement once, and a report with it alone when no allocation runs.
func TestTaskGroupUsageReporter_Host(t *testing.T) {
	ci.Parallel(t)

	r := newTaskGroupUsageReporter()
	start := time.Unix(1_700_000_000, 0)

	// Disabled: nothing is kept.
	r.observeHost(hostStats(1, 10, 10), 4000)
	r.setInterval(10*time.Second, start)
	r.windowDone("node", start.Add(10*time.Second))
	must.Nil(t, r.take())

	r.observeHost(hostStats(2, 20, 10), 4000) // 30% and 15%: 22.5%
	r.observeHost(hostStats(2, 99, 99), 4000) // the same measurement
	r.observeHost(hostStats(3, 40, 20), 4000) // 45%
	r.observeHost(&stats.HostStats{Timestamp: 4}, 4000)
	r.windowDone("node", start.Add(20*time.Second))
	report := r.take()
	must.NotNil(t, report)
	must.SliceEmpty(t, report.Groups)
	h := report.Host
	must.NotNil(t, h)
	must.Eq(t, 2, h.Samples)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 33.75, Max: 45}, h.CPUPercent)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 1350, Max: 1800}, h.CPUMHz)
	must.Eq(t, 4000.0, h.CPUTotalMHz)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 2048, Max: 2048}, h.MemoryUsedMB)
	must.Eq(t, 8192.0, h.MemoryTotalMB)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 1024, Max: 1024}, h.DiskUsedMB)
	must.Eq(t, 10240.0, h.DiskTotalMB)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 5, Max: 5}, h.InodesUsedPercent)

	// Without the CPU of the node no MHz; a measurement seen before is not
	// counted in the next window either.
	r.observeHost(hostStats(3, 40, 20), 0)
	r.observeHost(hostStats(5, 40, 20), 0)
	r.windowDone("node", start.Add(30*time.Second))
	h = r.take().Host
	must.Eq(t, 1, h.Samples)
	must.Eq(t, structs.TaskGroupUsageValue{}, h.CPUMHz)
	must.Eq(t, 0.0, h.CPUTotalMHz)

	// An empty window still produces no report.
	r.windowDone("node", start.Add(40*time.Second))
	must.Nil(t, r.take())
}

// TestClient_UsageDisabled checks that a client with usage disabled ignores
// the interval the servers ask for.
func TestClient_UsageDisabled(t *testing.T) {
	ci.Parallel(t)

	for _, disabled := range []bool{false, true} {
		c, cleanup := TestClient(t, func(cfg *config.Config) { cfg.UsageDisabled = disabled })
		_ = c.handleNodeUpdateResponse(structs.NodeUpdateResponse{TaskGroupUsageInterval: 10 * time.Second})
		want := 10 * time.Second
		if disabled {
			want = 0
		}
		must.Eq(t, want, c.taskGroupUsage.getInterval(), must.Sprintf("disabled=%t", disabled))
		cleanup()
	}
}
