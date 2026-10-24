// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// fakeUsage returns an alloc usage measurement with the given CPU (MHz) and
// memory (MiB).
func fakeUsage(cpuMHz float64, memoryMiB uint64) *cstructs.AllocResourceUsage {
	return &cstructs.AllocResourceUsage{
		ResourceUsage: &cstructs.ResourceUsage{
			CpuStats:    &cstructs.CpuStats{TotalTicks: cpuMHz},
			MemoryStats: &cstructs.MemoryStats{RSS: memoryMiB * 1024 * 1024},
		},
		Timestamp: 1,
	}
}

// usageAlloc returns an allocation of the task group with 500 MHz CPU and
// 256 MiB memory allocated (mock.Alloc's single web task).
func usageAlloc(job, group string) *structs.Allocation {
	a := mock.Alloc()
	a.JobID = job
	a.TaskGroup = group
	return a
}

func TestTaskGroupUsageAggregator_AvgMax(t *testing.T) {
	ci.Parallel(t)

	agg := NewTaskGroupUsageAggregator()
	a1 := usageAlloc("api", "web")
	a2 := usageAlloc("api", "web")
	must.Eq(t, 500, a1.AllocatedResources.Tasks["web"].Cpu.CpuShares)
	must.Eq(t, 256, a1.AllocatedResources.Tasks["web"].Memory.MemoryMB)

	// Two samples of two allocations.
	agg.Observe(a1, fakeUsage(100, 64))
	agg.Observe(a2, fakeUsage(300, 128))
	agg.Observe(a1, fakeUsage(200, 64))
	agg.Observe(a2, fakeUsage(400, 192))

	out := agg.Flush()
	must.Len(t, 1, out)
	u := out[0]
	must.Eq(t, a1.Namespace, u.Namespace)
	must.Eq(t, "api", u.JobID)
	must.Eq(t, "web", u.TaskGroup)
	must.Eq(t, 2, u.Allocs)
	must.Eq(t, 4, u.Samples)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 250, Max: 400}, u.CPUMHz)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 50, Max: 80}, u.CPUPercent)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 112, Max: 192}, u.MemoryMB)
	must.Eq(t, structs.TaskGroupUsageValue{Avg: 43.75, Max: 75}, u.MemoryPercent)

	// Flush starts a new window.
	must.Nil(t, agg.Flush())
}

func TestTaskGroupUsageAggregator_Groups(t *testing.T) {
	ci.Parallel(t)

	agg := NewTaskGroupUsageAggregator()
	agg.Observe(usageAlloc("b", "web"), fakeUsage(100, 10))
	agg.Observe(usageAlloc("a", "web"), fakeUsage(200, 20))
	agg.Observe(usageAlloc("a", "cache"), fakeUsage(300, 30))

	out := agg.Flush()
	must.Len(t, 3, out)
	must.Eq(t, "a", out[0].JobID)
	must.Eq(t, "cache", out[0].TaskGroup)
	must.Eq(t, 300.0, out[0].CPUMHz.Avg)
	must.Eq(t, "a", out[1].JobID)
	must.Eq(t, "web", out[1].TaskGroup)
	must.Eq(t, "b", out[2].JobID)
}

func TestTaskGroupUsageAggregator_IgnoresEmpty(t *testing.T) {
	ci.Parallel(t)

	agg := NewTaskGroupUsageAggregator()
	a := usageAlloc("api", "web")

	agg.Observe(nil, fakeUsage(1, 1))
	agg.Observe(a, nil)
	agg.Observe(a, &cstructs.AllocResourceUsage{ResourceUsage: &cstructs.ResourceUsage{}})
	must.Nil(t, agg.Flush())

	// Reset drops the samples of the current window.
	agg.Observe(a, fakeUsage(1, 1))
	agg.Reset()
	must.Nil(t, agg.Flush())
}

func TestTaskGroupUsageAggregator_MemoryUsagePreferred(t *testing.T) {
	ci.Parallel(t)

	agg := NewTaskGroupUsageAggregator()
	a := usageAlloc("api", "web")
	u := fakeUsage(0, 10)
	u.ResourceUsage.MemoryStats.Usage = 128 * 1024 * 1024
	u.ResourceUsage.CpuStats = nil
	agg.Observe(a, u)

	out := agg.Flush()
	must.Len(t, 1, out)
	must.Eq(t, 128.0, out[0].MemoryMB.Avg)
	must.Eq(t, 50.0, out[0].MemoryPercent.Avg)
	must.Eq(t, 0.0, out[0].CPUMHz.Max)
}

func TestTaskGroupUsageAggregator_NoAllocatedResources(t *testing.T) {
	ci.Parallel(t)

	agg := NewTaskGroupUsageAggregator()
	a := usageAlloc("api", "web")
	a.AllocatedResources = nil
	a.Resources = nil
	agg.Observe(a, fakeUsage(100, 10))

	out := agg.Flush()
	must.Len(t, 1, out)
	must.Eq(t, 100.0, out[0].CPUMHz.Avg)
	must.Eq(t, 0.0, out[0].CPUPercent.Avg)
}
