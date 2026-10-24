// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/shoenig/test/must"
)

func testQuotaSpec() *QuotaSpec {
	return &QuotaSpec{
		Name: "team-a",
		Limits: []*QuotaLimit{{
			Region:      "global",
			RegionLimit: &Resources{CPU: 1000, MemoryMB: 1000},
			Allocations: pointer.Of(5),
		}},
	}
}

func TestQuotaSpec_Validate(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name   string
		modify func(*QuotaSpec)
		errMsg string
	}{
		{name: "valid", modify: func(*QuotaSpec) {}},
		{name: "bad name", modify: func(q *QuotaSpec) { q.Name = "*" }, errMsg: "invalid name"},
		{name: "bad enforce", modify: func(q *QuotaSpec) { q.Enforce = "never" }, errMsg: "invalid enforce mode"},
		{name: "missing region", modify: func(q *QuotaSpec) { q.Limits[0].Region = "" }, errMsg: "missing region"},
		{
			name: "duplicate region",
			modify: func(q *QuotaSpec) {
				q.Limits = append(q.Limits, &QuotaLimit{Region: "global"})
			},
			errMsg: "duplicate limit",
		},
		{
			name:   "devices not supported",
			modify: func(q *QuotaSpec) { q.Limits[0].RegionLimit.Devices = []*RequestedDevice{{Name: "gpu"}} },
			errMsg: "device resources are not supported",
		},
		{
			name:   "cores limit not supported",
			modify: func(q *QuotaSpec) { q.Limits[0].RegionLimit.Cores = 2 },
			errMsg: "cores is not supported",
		},
		{
			name: "defaults above max",
			modify: func(q *QuotaSpec) {
				q.TaskDefaults = &Resources{CPU: 500}
				q.TaskMax = &Resources{CPU: 100}
			},
			errMsg: "cpu exceeds task_max",
		},
		{
			name:   "negative max",
			modify: func(q *QuotaSpec) { q.TaskMax = &Resources{MemoryMB: -1} },
			errMsg: "must not be negative",
		},
		{
			name: "negative region limit means disallowed",
			modify: func(q *QuotaSpec) {
				q.Limits[0].RegionLimit.CPU = -1
				q.Enforce = QuotaEnforceSubmit
				q.TaskDefaults = &Resources{CPU: 100, MemoryMB: 128, MemoryMaxMB: 256}
				q.TaskMax = &Resources{CPU: 1000, Cores: 2}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testQuotaSpec()
			tc.modify(spec)
			err := spec.Validate()
			if tc.errMsg == "" {
				must.NoError(t, err)
			} else {
				must.ErrorContains(t, err, tc.errMsg)
			}
		})
	}
}

func TestQuotaSpec_HashAndCopy(t *testing.T) {
	ci.Parallel(t)

	spec := testQuotaSpec()
	spec.Canonicalize()
	must.Eq(t, QuotaEnforceSchedule, spec.Enforce)

	h1 := spec.SetHash()
	limitHash := spec.Limits[0].Hash
	must.Len(t, 32, h1)
	must.Len(t, 32, limitHash)

	// Hashing is stable
	must.Eq(t, h1, spec.Copy().SetHash())

	// A copy is independent of the original
	cp := spec.Copy()
	cp.Limits[0].RegionLimit.CPU = 2000
	*cp.Limits[0].Allocations = 10
	must.Eq(t, 1000, spec.Limits[0].RegionLimit.CPU)
	must.Eq(t, 5, *spec.Limits[0].Allocations)

	// Changing a limit changes both hashes
	h2 := cp.SetHash()
	must.NotEq(t, h1, h2)
	must.NotEq(t, limitHash, cp.Limits[0].Hash)

	// Changing the description changes only the spec hash
	cp2 := spec.Copy()
	cp2.Description = "changed"
	must.NotEq(t, h1, cp2.SetHash())
	must.Eq(t, limitHash, cp2.Limits[0].Hash)
}

func TestQuotaLimit_Checks(t *testing.T) {
	ci.Parallel(t)

	limit := testQuotaSpec().Limits[0]
	used := QuotaResources{CPU: 800, MemoryMB: 500, Allocations: 4}

	// Fits
	must.SliceEmpty(t, limit.CheckAdditional(used, QuotaResources{CPU: 200, MemoryMB: 100, Allocations: 1}))

	// CPU and allocations exceeded
	dims := limit.CheckAdditional(used, QuotaResources{CPU: 300, MemoryMB: 100, Allocations: 2})
	must.Eq(t, []string{"cpu exhausted (1100 > 1000)", "allocations exhausted (6 > 5)"}, dims)

	// Reducing usage of an over-committed quota is allowed
	over := QuotaResources{CPU: 2000, MemoryMB: 500, Allocations: 4}
	must.SliceEmpty(t, limit.CheckAdditional(over, QuotaResources{CPU: -100, MemoryMB: -10, Allocations: -1}))
	// But growing a dimension of it is not
	must.Len(t, 1, limit.CheckAdditional(over, QuotaResources{CPU: 1}))

	// Exhausted means no room left
	must.Eq(t, []string{QuotaDimensionCPU}, limit.ExhaustedDimensions(QuotaResources{CPU: 1000}))
	must.Eq(t, []string{QuotaDimensionCPU, QuotaDimensionAllocations},
		limit.ExceededDimensions(QuotaResources{CPU: 1001, Allocations: 6}))

	// Negative limits disallow any usage
	disallowed := &QuotaLimit{Region: "global", RegionLimit: &Resources{MemoryMB: -1}}
	must.Len(t, 1, disallowed.CheckAdditional(QuotaResources{}, QuotaResources{MemoryMB: 1}))
	must.SliceEmpty(t, disallowed.CheckAdditional(QuotaResources{}, QuotaResources{CPU: 1}))
}

func TestQuotaLimit_Remaining(t *testing.T) {
	ci.Parallel(t)

	limit := testQuotaSpec().Limits[0]
	rem := limit.Remaining(QuotaResources{CPU: 400, MemoryMB: 900, Allocations: 1})
	must.Eq(t, int64(600), *rem.CPU)
	must.Eq(t, int64(100), *rem.MemoryMB)
	must.Nil(t, rem.MemoryMaxMB)
	must.Eq(t, int64(4), *rem.Allocations)

	// Memory is the binding dimension: 100 MB left / 50 MB per alloc
	must.Eq(t, 2, rem.MaxAllocs(QuotaResources{CPU: 100, MemoryMB: 50, Allocations: 1}))
	// Allocation count binds
	must.Eq(t, 4, rem.MaxAllocs(QuotaResources{CPU: 1, MemoryMB: 1, Allocations: 1}))

	var unlimited *QuotaRemaining
	must.Eq(t, -1, unlimited.MaxAllocs(QuotaResources{CPU: 1}))
	must.Eq(t, -1, (&QuotaLimit{}).Remaining(QuotaResources{}).MaxAllocs(QuotaResources{CPU: 1}))
}

func TestAllocQuotaResources(t *testing.T) {
	ci.Parallel(t)

	alloc := &Allocation{
		DesiredStatus: AllocDesiredStatusRun,
		ClientStatus:  AllocClientStatusRunning,
		AllocatedResources: &AllocatedResources{
			Tasks: map[string]*AllocatedTaskResources{
				"main": {
					Cpu:    AllocatedCpuResources{CpuShares: 500},
					Memory: AllocatedMemoryResources{MemoryMB: 256, MemoryMaxMB: 512},
				},
				"sidecar": {
					Cpu:    AllocatedCpuResources{CpuShares: 100},
					Memory: AllocatedMemoryResources{MemoryMB: 64},
				},
				"init": {
					Cpu:    AllocatedCpuResources{CpuShares: 1000},
					Memory: AllocatedMemoryResources{MemoryMB: 128},
				},
			},
			TaskLifecycles: map[string]*TaskLifecycleConfig{
				"sidecar": {Hook: TaskLifecycleHookPrestart, Sidecar: true},
				"init":    {Hook: TaskLifecycleHookPrestart},
			},
		},
	}

	// sidecar + max(init, main) per dimension
	must.Eq(t, QuotaResources{CPU: 1100, MemoryMB: 320, MemoryMaxMB: 576, Allocations: 1},
		AllocQuotaResources(alloc))

	alloc.DesiredStatus = AllocDesiredStatusStop
	must.True(t, AllocQuotaResources(alloc).IsZero())
	must.True(t, AllocQuotaResources(nil).IsZero())
}

func TestTaskGroupQuotaResources(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{
		Name:  "web",
		Count: 3,
		Tasks: []*Task{
			{Name: "a", Resources: &Resources{CPU: 200, MemoryMB: 100}},
			{Name: "b", Resources: &Resources{Cores: 2, MemoryMB: 100, MemoryMaxMB: 300}},
		},
	}
	must.True(t, TaskGroupUsesCores(tg))
	must.Eq(t, QuotaResources{CPU: 200 + 2*1500, MemoryMB: 200, MemoryMaxMB: 400, Allocations: 1},
		TaskGroupQuotaResources(tg, 1500))

	job := &Job{Type: JobTypeService, TaskGroups: []*TaskGroup{tg}}
	must.Eq(t, QuotaResources{CPU: 600, MemoryMB: 600, MemoryMaxMB: 1200, Allocations: 3}, JobQuotaDemand(job))

	job.Type = JobTypeSystem
	must.True(t, JobQuotaDemand(job).IsZero())
}

func TestQuotaPlanDelta(t *testing.T) {
	ci.Parallel(t)

	newAlloc := func(id, ns string, cpu int64) *Allocation {
		return &Allocation{
			ID:            id,
			Namespace:     ns,
			DesiredStatus: AllocDesiredStatusRun,
			ClientStatus:  AllocClientStatusPending,
			AllocatedResources: &AllocatedResources{
				Tasks: map[string]*AllocatedTaskResources{
					"web": {Cpu: AllocatedCpuResources{CpuShares: cpu}, Memory: AllocatedMemoryResources{MemoryMB: 10}},
				},
			},
		}
	}

	existing := map[string]*Allocation{
		"stop":     newAlloc("stop", "a", 100),
		"inplace":  newAlloc("inplace", "a", 100),
		"preempt":  newAlloc("preempt", "b", 100),
		"terminal": newAlloc("terminal", "a", 100),
	}
	existing["terminal"].ClientStatus = AllocClientStatusFailed

	stopped := func(a *Allocation) *Allocation {
		c := a.Copy()
		c.DesiredStatus = AllocDesiredStatusStop
		return c
	}

	plan := &Plan{
		Job: &Job{Namespace: "a"},
		NodeUpdate: map[string][]*Allocation{
			"n1": {stopped(existing["stop"]), stopped(existing["terminal"])},
		},
		NodePreemptions: map[string][]*Allocation{
			"n1": {stopped(existing["preempt"])},
		},
		NodeAllocation: map[string][]*Allocation{
			"n1": {newAlloc("new1", "a", 300), newAlloc("inplace", "a", 150)},
		},
	}

	var lookups int
	delta, err := QuotaPlanDelta(plan,
		func(ns string) (bool, error) { return ns == "a", nil },
		func(id string) (QuotaResources, error) {
			lookups++
			return AllocQuotaResources(existing[id]), nil
		})
	must.NoError(t, err)

	// -100 (stop) + 300 (new) + 50 (in-place growth); preemption in another
	// namespace and the already terminal alloc don't count.
	must.Eq(t, QuotaResources{CPU: 250}, delta)
	must.Eq(t, 4, lookups)
}

func TestQuotaUsage_Entries(t *testing.T) {
	ci.Parallel(t)

	spec := testQuotaSpec()
	spec.Limits = append(spec.Limits, &QuotaLimit{Region: "other", RegionLimit: &Resources{CPU: 1}})
	spec.SetHash()

	usage := NewQuotaUsage(spec, "global")
	must.MapLen(t, 1, usage.Used)
	entry := usage.RegionUsage("global")
	must.NotNil(t, entry)
	must.Eq(t, spec.Limits[0].Hash, entry.Hash)
	must.NotNil(t, usage.Used[spec.Limits[0].HashKey()])
	must.Nil(t, usage.RegionUsage("other"))

	entry.SetUsageResources(QuotaResources{CPU: 10, MemoryMB: 20, MemoryMaxMB: 30, Allocations: 2})
	cp := usage.Copy()
	cp.RegionUsage("global").SetUsageResources(QuotaResources{})
	must.Eq(t, QuotaResources{CPU: 10, MemoryMB: 20, MemoryMaxMB: 30, Allocations: 2}, entry.UsageResources())
}
