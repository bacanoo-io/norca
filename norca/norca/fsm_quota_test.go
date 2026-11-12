// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestFSM_QuotaSpecs(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	q := mockQuotaSpec()
	q.Canonicalize()
	q.SetHash()

	// Sent with the ignore flag, as the RPC endpoint does
	buf, err := structs.Encode(structs.QuotaSpecUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		structs.QuotaSpecUpsertRequest{Quotas: []*structs.QuotaSpec{q}})
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	out, err := fsm.State().QuotaSpecByName(nil, q.Name)
	must.NoError(t, err)
	must.NotNil(t, out)

	// allocQuota resolves the quota through the alloc's namespace
	ns := mock.Namespace()
	ns.Quota = q.Name
	must.NoError(t, fsm.State().UpsertNamespaces(10, []*structs.Namespace{ns}))
	job := mock.Job()
	job.Namespace = ns.Name
	must.NoError(t, fsm.State().UpsertJob(structs.MsgTypeTestSetup, 11, nil, job))
	alloc := mock.Alloc()
	alloc.Namespace = ns.Name
	alloc.Job = job
	alloc.JobID = job.ID
	must.NoError(t, fsm.State().UpsertAllocs(structs.MsgTypeTestSetup, 12, []*structs.Allocation{alloc}))

	quota, err := fsm.allocQuota(alloc.ID)
	must.NoError(t, err)
	must.Eq(t, q.Name, quota)
	quota, err = fsm.allocQuota(uuid.Generate())
	must.NoError(t, err)
	must.Eq(t, "", quota)

	// Snapshot and restore keep specs and usage
	fsm2 := testSnapshotRestore(t, fsm)
	out2, err := fsm2.State().QuotaSpecByName(nil, q.Name)
	must.NoError(t, err)
	must.Eq(t, out, out2)
	usage, err := fsm2.State().QuotaUsageByName(nil, q.Name)
	must.NoError(t, err)
	must.Eq(t, int64(1), usage.RegionUsage("global").UsageResources().Allocations)

	// Delete fails while in use
	buf, err = structs.Encode(structs.QuotaSpecDeleteRequestType|structs.IgnoreUnknownTypeFlag,
		structs.QuotaSpecDeleteRequest{Names: []string{q.Name}})
	must.NoError(t, err)
	resp := fsm.Apply(makeLog(buf))
	must.NotNil(t, resp)
	must.ErrorContains(t, resp.(error), "in use")
}

func TestPlanApply_EvaluatePlanQuota(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)
	state := fsm.State()

	q := mockQuotaSpec()
	q.Limits[0].RegionLimit = &structs.Resources{CPU: 1000}
	q.Limits[0].Allocations = pointer.Of(2)
	q.Canonicalize()
	q.SetHash()
	must.NoError(t, state.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 10, []*structs.QuotaSpec{q}))

	ns := mock.Namespace()
	ns.Quota = q.Name
	must.NoError(t, state.UpsertNamespaces(11, []*structs.Namespace{ns}))

	job := mock.Job()
	job.Namespace = ns.Name
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 12, nil, job))

	newAlloc := func() *structs.Allocation {
		a := mock.Alloc()
		a.Namespace = ns.Name
		a.Job = job
		a.JobID = job.ID
		return a
	}
	existing := newAlloc()
	must.NoError(t, state.UpsertAllocs(structs.MsgTypeTestSetup, 13, []*structs.Allocation{existing}))

	snap, err := state.Snapshot()
	must.NoError(t, err)

	// One more 500 MHz allocation fits
	plan := &structs.Plan{
		Job:            job,
		NodeAllocation: map[string][]*structs.Allocation{"n1": {newAlloc()}},
	}
	over, err := evaluatePlanQuota(snap, plan)
	must.NoError(t, err)
	must.False(t, over)

	// Two do not
	plan.NodeAllocation["n1"] = append(plan.NodeAllocation["n1"], newAlloc())
	dims, err := planQuotaViolations(snap, plan)
	must.NoError(t, err)
	must.Eq(t, []string{"cpu exhausted (1500 > 1000)", "allocations exhausted (3 > 2)"}, dims)

	// Unless the existing allocation is stopped by the same plan
	stopped := existing.Copy()
	stopped.DesiredStatus = structs.AllocDesiredStatusStop
	plan.NodeUpdate = map[string][]*structs.Allocation{"n1": {stopped}}
	over, err = evaluatePlanQuota(snap, plan)
	must.NoError(t, err)
	must.False(t, over)

	// Jobs in namespaces without a quota are never rejected
	other := mock.Job()
	over, err = evaluatePlanQuota(snap, &structs.Plan{
		Job:            other,
		NodeAllocation: map[string][]*structs.Allocation{"n1": {mock.Alloc(), mock.Alloc(), mock.Alloc()}},
	})
	must.NoError(t, err)
	must.False(t, over)
}

func TestLeader_DiffQuotaSpecs(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)
	state := fsm.State()

	same, changed, deleted := mockQuotaSpec(), mockQuotaSpec(), mockQuotaSpec()
	for _, q := range []*structs.QuotaSpec{same, changed, deleted} {
		q.SetHash()
	}
	must.NoError(t, state.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 100, []*structs.QuotaSpec{same, changed, deleted}))

	remoteSame := same.Copy()
	remoteSame.ModifyIndex = 50
	remoteChanged := changed.Copy()
	remoteChanged.Description = "changed"
	remoteChanged.SetHash()
	remoteChanged.ModifyIndex = 60
	added := mockQuotaSpec()
	added.SetHash()
	added.ModifyIndex = 70

	del, update := diffQuotaSpecs(state, 55, []*structs.QuotaSpec{remoteSame, remoteChanged, added})
	must.Eq(t, []string{deleted.Name}, del)
	must.Len(t, 2, update)
	names := []string{update[0].Name, update[1].Name}
	must.SliceContainsAll(t, []string{changed.Name, added.Name}, names)
}

// A plan that is applied in part is checked against the quota for the part
// that is applied: a node that is refused takes the stops of the plan on it
// with it, and what is left may add to the quota.
func TestPlanApply_EvaluatePlan_QuotaOfPartialCommit(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)
	state := fsm.State()

	q := mockQuotaSpec()
	q.Limits[0].RegionLimit = nil
	q.Limits[0].Allocations = pointer.Of(2)
	q.Canonicalize()
	q.SetHash()
	must.NoError(t, state.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 10, []*structs.QuotaSpec{q}))

	ns := mock.Namespace()
	ns.Quota = q.Name
	must.NoError(t, state.UpsertNamespaces(11, []*structs.Namespace{ns}))

	job := mock.Job()
	job.Namespace = ns.Name
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 12, nil, job))

	full, other := mock.Node(), mock.Node()
	must.NoError(t, state.UpsertNode(structs.MsgTypeTestSetup, 13, full))
	must.NoError(t, state.UpsertNode(structs.MsgTypeTestSetup, 14, other))

	newAlloc := func(node *structs.Node) *structs.Allocation {
		a := mock.Alloc()
		a.Namespace = ns.Name
		a.Job = job
		a.JobID = job.ID
		a.NodeID = node.ID
		return a
	}

	// The quota is full: two allocations run on the first node.
	a1, a2 := newAlloc(full), newAlloc(full)
	must.NoError(t, state.UpsertAllocs(structs.MsgTypeTestSetup, 15, []*structs.Allocation{a1, a2}))
	snap, err := state.Snapshot()
	must.NoError(t, err)

	stop := func(a *structs.Allocation) *structs.Allocation {
		s := a.Copy()
		s.DesiredStatus = structs.AllocDesiredStatusStop
		return s
	}
	// The plan stops both and places two: one that does not fit its node,
	// and one on the other node. As a whole it adds nothing to the quota.
	tooBig := newAlloc(full)
	tooBig.AllocatedResources = structs.NodeResourcesToAllocatedResources(full.NodeResources)
	tooBig.AllocatedResources.Tasks["web"].Cpu.CpuShares *= 2
	placed := newAlloc(other)
	plan := &structs.Plan{
		Job:        job,
		NodeUpdate: map[string][]*structs.Allocation{full.ID: {stop(a1), stop(a2)}},
		NodeAllocation: map[string][]*structs.Allocation{
			full.ID:  {tooBig},
			other.ID: {placed},
		},
	}
	over, err := evaluatePlanQuota(snap, plan)
	must.NoError(t, err)
	must.False(t, over)

	pool := NewEvaluatePool(workerPoolSize, workerPoolBufferSize)
	defer pool.Shutdown()
	result, err := evaluatePlan(pool, snap, plan, testlog.HCLogger(t))
	must.NoError(t, err)
	must.NotNil(t, result)

	// The first node is refused with its stops. The placement on the other
	// node alone would be the third allocation of the quota: nothing of
	// the plan is applied.
	must.NonZero(t, result.RefreshIndex)
	must.MapEmpty(t, result.NodeAllocation)
	must.MapEmpty(t, result.NodeUpdate)
}

// The refresh index of a plan that is over its quota covers the binding of a
// namespace to a quota.
func TestPlanApply_RefreshIndex_NamespaceQuota(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)
	state := fsm.State()

	q := mockQuotaSpec()
	q.Canonicalize()
	q.SetHash()
	must.NoError(t, state.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 10, []*structs.QuotaSpec{q}))
	ns := mock.Namespace()
	must.NoError(t, state.UpsertNamespaces(11, []*structs.Namespace{ns}))

	bound := ns.Copy()
	bound.Quota = q.Name
	must.NoError(t, state.UpsertNamespaces(20, []*structs.Namespace{bound}))

	snap, err := state.Snapshot()
	must.NoError(t, err)
	index, err := refreshIndex(snap)
	must.NoError(t, err)
	must.Eq(t, 20, index)
}
