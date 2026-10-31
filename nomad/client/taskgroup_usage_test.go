// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
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
}
