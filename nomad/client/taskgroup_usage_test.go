// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/mock"
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
