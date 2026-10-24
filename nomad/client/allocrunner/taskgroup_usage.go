// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"sort"
	"sync"

	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TaskGroupUsageAggregator aggregates the resource usage measurements
// (AllocResourceUsage) of the allocations running on a client per task group
// over a window. Each measurement of an allocation is one sample; Flush
// returns the average and the maximum per allocation of every task group
// since the previous Flush. The built-in autoscaler of the servers uses the
// result as its nomad.cpu and nomad.memory metrics.
//
// It is safe for concurrent use.
type TaskGroupUsageAggregator struct {
	l      sync.Mutex
	groups map[taskGroupUsageKey]*taskGroupUsageAcc
}

type taskGroupUsageKey struct {
	namespace string
	job       string
	group     string
}

// taskGroupUsageAcc accumulates the samples of one task group.
type taskGroupUsageAcc struct {
	allocs  map[string]struct{}
	samples int

	cpuMHz, cpuPercent, memoryMB, memoryPercent usageAcc
}

// usageAcc accumulates the sum and maximum of a value.
type usageAcc struct {
	sum float64
	max float64
}

func (u *usageAcc) add(v float64) {
	u.sum += v
	if v > u.max {
		u.max = v
	}
}

func (u *usageAcc) value(samples int) structs.TaskGroupUsageValue {
	return structs.TaskGroupUsageValue{Avg: u.sum / float64(samples), Max: u.max}
}

// NewTaskGroupUsageAggregator returns an empty aggregator.
func NewTaskGroupUsageAggregator() *TaskGroupUsageAggregator {
	return &TaskGroupUsageAggregator{groups: map[taskGroupUsageKey]*taskGroupUsageAcc{}}
}

// Observe adds one usage measurement of a running allocation. Measurements
// without data (the tasks have not reported statistics yet) are ignored.
func (a *TaskGroupUsageAggregator) Observe(alloc *structs.Allocation, usage *cstructs.AllocResourceUsage) {
	if alloc == nil || usage == nil || usage.ResourceUsage == nil || usage.Timestamp == 0 {
		return
	}

	cpuMHz, memoryMB := allocUsage(usage.ResourceUsage)
	allocCPU, allocMemory := allocatedCPUAndMemory(alloc)

	key := taskGroupUsageKey{namespace: alloc.Namespace, job: alloc.JobID, group: alloc.TaskGroup}

	a.l.Lock()
	defer a.l.Unlock()

	acc, ok := a.groups[key]
	if !ok {
		acc = &taskGroupUsageAcc{allocs: map[string]struct{}{}}
		a.groups[key] = acc
	}
	acc.allocs[alloc.ID] = struct{}{}
	acc.samples++
	acc.cpuMHz.add(cpuMHz)
	acc.memoryMB.add(memoryMB)
	acc.cpuPercent.add(percentOf(cpuMHz, allocCPU))
	acc.memoryPercent.add(percentOf(memoryMB, allocMemory))
}

// Flush returns the usage of every task group observed since the previous
// Flush, sorted by namespace, job and group, and starts a new window.
func (a *TaskGroupUsageAggregator) Flush() []*structs.TaskGroupUsage {
	a.l.Lock()
	groups := a.groups
	a.groups = make(map[taskGroupUsageKey]*taskGroupUsageAcc, len(groups))
	a.l.Unlock()

	if len(groups) == 0 {
		return nil
	}

	out := make([]*structs.TaskGroupUsage, 0, len(groups))
	for key, acc := range groups {
		out = append(out, &structs.TaskGroupUsage{
			Namespace:     key.namespace,
			JobID:         key.job,
			TaskGroup:     key.group,
			Allocs:        len(acc.allocs),
			Samples:       acc.samples,
			CPUMHz:        acc.cpuMHz.value(acc.samples),
			CPUPercent:    acc.cpuPercent.value(acc.samples),
			MemoryMB:      acc.memoryMB.value(acc.samples),
			MemoryPercent: acc.memoryPercent.value(acc.samples),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		if x.JobID != y.JobID {
			return x.JobID < y.JobID
		}
		return x.TaskGroup < y.TaskGroup
	})
	return out
}

// Reset drops the samples of the current window.
func (a *TaskGroupUsageAggregator) Reset() {
	a.l.Lock()
	defer a.l.Unlock()
	a.groups = map[taskGroupUsageKey]*taskGroupUsageAcc{}
}

// allocUsage returns the CPU (MHz) and memory (MiB) usage of an allocation.
// Memory is the cgroup usage when the driver measures it, otherwise the RSS,
// the same value the nomad-apm of the external autoscaler uses.
func allocUsage(ru *cstructs.ResourceUsage) (cpuMHz, memoryMB float64) {
	if ru.CpuStats != nil {
		cpuMHz = ru.CpuStats.TotalTicks
	}
	if ms := ru.MemoryStats; ms != nil {
		bytes := ms.Usage
		if bytes == 0 {
			bytes = ms.RSS
		}
		memoryMB = float64(bytes) / (1024 * 1024)
	}
	return cpuMHz, memoryMB
}

// allocatedCPUAndMemory returns the CPU (MHz) and memory (MiB) allocated to
// all tasks of an allocation.
func allocatedCPUAndMemory(alloc *structs.Allocation) (cpuMHz, memoryMB float64) {
	if alloc.AllocatedResources == nil && alloc.Resources == nil {
		return 0, 0
	}
	res := alloc.ComparableResources()
	if res == nil {
		return 0, 0
	}
	return float64(res.Flattened.Cpu.CpuShares), float64(res.Flattened.Memory.MemoryMB)
}

// percentOf returns used as a percentage of allocated, or 0 if nothing is
// allocated.
func percentOf(used, allocated float64) float64 {
	if allocated <= 0 {
		return 0
	}
	return used / allocated * 100
}
