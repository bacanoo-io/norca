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

	// allocs is the previous measurement of each allocation, for the
	// throttling between two measurements and the OOM kills seen.
	allocs map[string]*allocUsagePrev
	// seen are the allocations observed in the window; the others are
	// forgotten at Flush.
	seen map[string]struct{}
}

// allocUsagePrev is what the aggregator remembers of an allocation.
type allocUsagePrev struct {
	timestamp     int64 // of the previous measurement (ns)
	throttledTime uint64
	// lastOOM is the time of the newest OOM kill event counted (ns).
	lastOOM int64
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

	cpuMHz, cpuPercent, memoryMB, memoryPercent, memoryCacheMB usageAcc

	// throttled has its own samples: it needs two measurements.
	throttled        usageAcc
	throttledSamples int

	oomKills int
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
	if samples == 0 {
		return structs.TaskGroupUsageValue{}
	}
	return structs.TaskGroupUsageValue{Avg: u.sum / float64(samples), Max: u.max}
}

// NewTaskGroupUsageAggregator returns an empty aggregator.
func NewTaskGroupUsageAggregator() *TaskGroupUsageAggregator {
	return &TaskGroupUsageAggregator{
		groups: map[taskGroupUsageKey]*taskGroupUsageAcc{},
		allocs: map[string]*allocUsagePrev{},
		seen:   map[string]struct{}{},
	}
}

// group returns the accumulator of the task group of alloc; a.l is held.
func (a *TaskGroupUsageAggregator) group(alloc *structs.Allocation) *taskGroupUsageAcc {
	key := taskGroupUsageKey{namespace: alloc.Namespace, job: alloc.JobID, group: alloc.TaskGroup}
	acc, ok := a.groups[key]
	if !ok {
		acc = &taskGroupUsageAcc{allocs: map[string]struct{}{}}
		a.groups[key] = acc
	}
	return acc
}

// prev returns what the aggregator remembers of alloc; a.l is held.
func (a *TaskGroupUsageAggregator) prev(alloc *structs.Allocation) *allocUsagePrev {
	a.seen[alloc.ID] = struct{}{}
	p := a.allocs[alloc.ID]
	if p == nil {
		p = &allocUsagePrev{}
		a.allocs[alloc.ID] = p
	}
	return p
}

// ObserveTaskStates counts the OOM kills among the task events of a running
// allocation that are newer than those counted before. The first call for
// an allocation counts the kills of its whole history still in the events.
func (a *TaskGroupUsageAggregator) ObserveTaskStates(alloc *structs.Allocation, states map[string]*structs.TaskState) {
	if alloc == nil {
		return
	}
	a.l.Lock()
	defer a.l.Unlock()
	p := a.prev(alloc)
	newest := p.lastOOM
	kills := 0
	for _, ts := range states {
		if ts == nil {
			continue
		}
		for _, e := range ts.Events {
			if e == nil || e.Time <= p.lastOOM || e.Details["oom_killed"] != "true" {
				continue
			}
			kills++
			newest = max(newest, e.Time)
		}
	}
	p.lastOOM = newest
	if kills > 0 {
		a.group(alloc).oomKills += kills
	}
}

// Observe adds one usage measurement of a running allocation. Measurements
// without data (the tasks have not reported statistics yet) are ignored.
func (a *TaskGroupUsageAggregator) Observe(alloc *structs.Allocation, usage *cstructs.AllocResourceUsage) {
	if alloc == nil || usage == nil || usage.ResourceUsage == nil || usage.Timestamp == 0 {
		return
	}

	cpuMHz, memoryMB := allocUsage(usage.ResourceUsage)
	allocCPU, allocMemory := allocatedCPUAndMemory(alloc)
	var cacheMB float64
	var throttledTime uint64
	if ms := usage.ResourceUsage.MemoryStats; ms != nil {
		cacheMB = float64(ms.Cache) / (1024 * 1024)
	}
	if cs := usage.ResourceUsage.CpuStats; cs != nil {
		throttledTime = cs.ThrottledTime
	}

	a.l.Lock()
	defer a.l.Unlock()

	acc := a.group(alloc)
	acc.allocs[alloc.ID] = struct{}{}
	acc.samples++
	acc.cpuMHz.add(cpuMHz)
	acc.memoryMB.add(memoryMB)
	acc.memoryCacheMB.add(cacheMB)
	acc.cpuPercent.add(percentOf(cpuMHz, allocCPU))
	acc.memoryPercent.add(percentOf(memoryMB, allocMemory))

	// The throttled time is a counter: its increase over the time between
	// two measurements. A counter that went back (a task restarted) starts
	// over.
	p := a.prev(alloc)
	if p.timestamp > 0 && usage.Timestamp > p.timestamp && throttledTime >= p.throttledTime {
		elapsed := float64(usage.Timestamp - p.timestamp)
		acc.throttled.add(float64(throttledTime-p.throttledTime) / elapsed * 100)
		acc.throttledSamples++
	}
	if usage.Timestamp > p.timestamp {
		p.timestamp, p.throttledTime = usage.Timestamp, throttledTime
	}
}

// Flush returns the usage of every task group observed since the previous
// Flush, sorted by namespace, job and group, and starts a new window.
func (a *TaskGroupUsageAggregator) Flush() []*structs.TaskGroupUsage {
	a.l.Lock()
	groups := a.groups
	a.groups = make(map[taskGroupUsageKey]*taskGroupUsageAcc, len(groups))
	for id := range a.allocs {
		if _, ok := a.seen[id]; !ok {
			delete(a.allocs, id) // stopped or gone
		}
	}
	a.seen = make(map[string]struct{}, len(a.allocs))
	a.l.Unlock()

	if len(groups) == 0 {
		return nil
	}

	out := make([]*structs.TaskGroupUsage, 0, len(groups))
	for key, acc := range groups {
		if acc.samples == 0 && acc.oomKills == 0 {
			continue
		}
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
			MemoryCacheMB: acc.memoryCacheMB.value(acc.samples),

			CPUThrottledPercent: acc.throttled.value(acc.throttledSamples),
			OOMKills:            acc.oomKills,
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

// Reset drops the samples of the current window and what it remembers of
// the allocations.
func (a *TaskGroupUsageAggregator) Reset() {
	a.l.Lock()
	defer a.l.Unlock()
	a.groups = map[taskGroupUsageKey]*taskGroupUsageAcc{}
	a.allocs = map[string]*allocUsagePrev{}
	a.seen = map[string]struct{}{}
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
