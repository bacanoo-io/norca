// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// quotaHarness creates a harness with nodes and a namespace limited by a
// quota with the given limit.
func quotaHarness(t *testing.T, limit *structs.QuotaLimit) (*Harness, *structs.Namespace) {
	h := NewHarness(t)
	for i := 0; i < 5; i++ {
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), mock.Node()))
	}

	limit.Region = "global"
	spec := &structs.QuotaSpec{Name: "q-" + uuid.Short(), Limits: []*structs.QuotaLimit{limit}}
	spec.Canonicalize()
	spec.SetHash()
	must.NoError(t, h.State.UpsertQuotaSpecs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.QuotaSpec{spec}))

	ns := mock.Namespace()
	ns.Quota = spec.Name
	must.NoError(t, h.State.UpsertNamespaces(h.NextIndex(), []*structs.Namespace{ns}))
	return h, ns
}

func quotaRegisterEval(t *testing.T, h *Harness, job *structs.Job) *structs.Evaluation {
	eval := &structs.Evaluation{
		Namespace:   job.Namespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	return eval
}

func plannedAllocs(plan *structs.Plan) []*structs.Allocation {
	var out []*structs.Allocation
	for _, allocs := range plan.NodeAllocation {
		out = append(out, allocs...)
	}
	return out
}

func TestQuotaIterator_PlacesUpToLimit(t *testing.T) {
	ci.Parallel(t)

	// 3 allocations of 500 MHz fit into 1500 MHz
	h, ns := quotaHarness(t, &structs.QuotaLimit{RegionLimit: &structs.Resources{CPU: 1500}})

	job := mock.Job()
	job.Namespace = ns.Name
	job.TaskGroups[0].Count = 5
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	eval := quotaRegisterEval(t, h, job)
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	must.Len(t, 1, h.Plans)
	must.Len(t, 3, plannedAllocs(h.Plans[0]))

	// The remaining placements are blocked on the quota
	must.Len(t, 1, h.CreateEvals)
	blocked := h.CreateEvals[0]
	must.Eq(t, structs.EvalStatusBlocked, blocked.Status)
	must.Eq(t, ns.Quota, blocked.QuotaLimitReached)

	must.Len(t, 1, h.Evals)
	metrics := h.Evals[0].FailedTGAllocs[job.TaskGroups[0].Name]
	must.NotNil(t, metrics)
	must.Eq(t, 2, metrics.CoalescedFailures+1)
	must.Eq(t, []string{"cpu exhausted (2000 > 1500)"}, metrics.QuotaExhausted)
}

func TestQuotaIterator_AllocationCount(t *testing.T) {
	ci.Parallel(t)

	h, ns := quotaHarness(t, &structs.QuotaLimit{Allocations: pointer.Of(2)})

	job := mock.BatchJob()
	job.Namespace = ns.Name
	job.TaskGroups[0].Count = 4
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	eval := quotaRegisterEval(t, h, job)
	must.NoError(t, h.Process(NewBatchScheduler, eval))

	must.Len(t, 1, h.Plans)
	must.Len(t, 2, plannedAllocs(h.Plans[0]))
	must.Len(t, 1, h.CreateEvals)
	must.Eq(t, ns.Quota, h.CreateEvals[0].QuotaLimitReached)
}

func TestQuotaIterator_DestructiveUpdateAtLimit(t *testing.T) {
	ci.Parallel(t)

	// The quota is fully used by the existing allocations; replacing them
	// must still be possible since the old ones are stopped in the same plan.
	h, ns := quotaHarness(t, &structs.QuotaLimit{RegionLimit: &structs.Resources{CPU: 1500}})

	job := mock.Job()
	job.Namespace = ns.Name
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Update = nil
	job.Update = structs.UpdateStrategy{}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	nodes, err := h.State.Nodes(nil)
	must.NoError(t, err)
	node := nodes.Next().(*structs.Node)

	var allocs []*structs.Allocation
	for i := 0; i < 3; i++ {
		alloc := mock.Alloc()
		alloc.Namespace = ns.Name
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Destructive change
	job2 := job.Copy()
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	eval := quotaRegisterEval(t, h, job2)
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	must.Len(t, 1, h.Plans)
	must.Len(t, 3, plannedAllocs(h.Plans[0]))
	must.Len(t, 0, h.CreateEvals)
}

func TestQuotaIterator_Cores(t *testing.T) {
	ci.Parallel(t)

	// mock nodes have 1000 MHz per core: two 1-core allocations fit into
	// 2500 MHz, the third does not.
	h, ns := quotaHarness(t, &structs.QuotaLimit{RegionLimit: &structs.Resources{CPU: 2500}})
	for i := 0; i < 3; i++ {
		node := mock.Node()
		node.NodeResources.Cpu.TotalCpuCores = 4
		node.NodeResources.Cpu.ReservableCpuCores = []uint16{0, 1, 2, 3}
		node.ComputeClass()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	job := mock.Job()
	job.Namespace = ns.Name
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Tasks[0].Resources.CPU = 0
	job.TaskGroups[0].Tasks[0].Resources.Cores = 1
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	eval := quotaRegisterEval(t, h, job)
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	must.Len(t, 1, h.Plans)
	must.Len(t, 2, plannedAllocs(h.Plans[0]))
	must.Len(t, 1, h.CreateEvals)
	must.Eq(t, ns.Quota, h.CreateEvals[0].QuotaLimitReached)
}

func TestQuotaIterator_NoQuota(t *testing.T) {
	ci.Parallel(t)

	// Namespaces without a quota are not limited
	h, _ := quotaHarness(t, &structs.QuotaLimit{Allocations: pointer.Of(1)})

	job := mock.Job()
	job.TaskGroups[0].Count = 4
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	eval := quotaRegisterEval(t, h, job)
	must.NoError(t, h.Process(NewServiceScheduler, eval))

	must.Len(t, 1, h.Plans)
	must.Len(t, 4, plannedAllocs(h.Plans[0]))
	must.Len(t, 0, h.CreateEvals)
}

func TestQuotaIterator_SystemJob(t *testing.T) {
	ci.Parallel(t)

	h, ns := quotaHarness(t, &structs.QuotaLimit{Allocations: pointer.Of(2)})

	job := mock.SystemJob()
	job.Namespace = ns.Name
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	eval := quotaRegisterEval(t, h, job)
	must.NoError(t, h.Process(NewSystemScheduler, eval))

	must.Len(t, 1, h.Plans)
	must.Len(t, 2, plannedAllocs(h.Plans[0]))
	must.Len(t, 1, h.CreateEvals)
	must.Eq(t, ns.Quota, h.CreateEvals[0].QuotaLimitReached)
}
