// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"
	"time"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testQuotaSpec(name string) *structs.QuotaSpec {
	spec := &structs.QuotaSpec{
		Name: name,
		Limits: []*structs.QuotaLimit{{
			Region:         "global",
			RegionLimit:    &structs.Resources{CPU: 2000, MemoryMB: 1024},
			Allocations:    pointer.Of(3),
			VariablesLimit: pointer.Of(1),
		}},
	}
	spec.Canonicalize()
	spec.SetHash()
	return spec
}

// quotaTestSetup creates a namespace using a new quota and a job in it.
func quotaTestSetup(t *testing.T, s *StateStore) (*structs.QuotaSpec, *structs.Namespace, *structs.Job) {
	spec := testQuotaSpec("q-" + uuid.Short())
	must.NoError(t, s.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 100, []*structs.QuotaSpec{spec}))

	ns := mock.Namespace()
	ns.Quota = spec.Name
	must.NoError(t, s.UpsertNamespaces(101, []*structs.Namespace{ns}))

	job := mock.Job()
	job.Namespace = ns.Name
	must.NoError(t, s.UpsertJob(structs.MsgTypeTestSetup, 102, nil, job))
	return spec, ns, job
}

func quotaTestAlloc(job *structs.Job) *structs.Allocation {
	alloc := mock.Alloc()
	alloc.Namespace = job.Namespace
	alloc.Job = job
	alloc.JobID = job.ID
	return alloc
}

func quotaUsed(t *testing.T, s *StateStore, name string) structs.QuotaResources {
	usage, err := s.QuotaUsageByName(nil, name)
	must.NoError(t, err)
	must.NotNil(t, usage)
	entry := usage.RegionUsage("global")
	must.NotNil(t, entry)
	return entry.UsageResources()
}

func TestStateStore_QuotaSpecs_CRUD(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)

	spec1 := testQuotaSpec("alpha")
	spec2 := testQuotaSpec("beta")
	must.NoError(t, s.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 10, []*structs.QuotaSpec{spec1, spec2}))

	ws := memdb.NewWatchSet()
	out, err := s.QuotaSpecByName(ws, "alpha")
	must.NoError(t, err)
	must.Eq(t, uint64(10), out.CreateIndex)
	must.Eq(t, spec1.Hash, out.Hash)

	iter, err := s.QuotaSpecsByNamePrefix(nil, "be", SortDefault)
	must.NoError(t, err)
	must.Eq(t, "beta", iter.Next().(*structs.QuotaSpec).Name)
	must.Nil(t, iter.Next())

	// Every spec gets an empty usage with one entry for the local region
	usage, err := s.QuotaUsageByName(nil, "alpha")
	must.NoError(t, err)
	must.MapLen(t, 1, usage.Used)
	must.NotNil(t, usage.Used[spec1.Limits[0].HashKey()])

	// Update fires the watch and keeps the create index
	update := spec1.Copy()
	update.Description = "updated"
	update.SetHash()
	must.NoError(t, s.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 11, []*structs.QuotaSpec{update}))
	must.True(t, watchFired(ws))
	out, err = s.QuotaSpecByName(nil, "alpha")
	must.NoError(t, err)
	must.Eq(t, uint64(10), out.CreateIndex)
	must.Eq(t, uint64(11), out.ModifyIndex)
	must.Eq(t, "updated", out.Description)

	idx, err := s.Index(TableQuotaSpecs)
	must.NoError(t, err)
	must.Eq(t, uint64(11), idx)

	// A namespace can't use a missing quota
	ns := mock.Namespace()
	ns.Quota = "missing"
	must.ErrorContains(t, s.UpsertNamespaces(12, []*structs.Namespace{ns}), "non-existent quota")

	// A quota in use can't be deleted
	ns.Quota = "alpha"
	must.NoError(t, s.UpsertNamespaces(13, []*structs.Namespace{ns}))
	must.ErrorContains(t, s.DeleteQuotaSpecs(structs.MsgTypeTestSetup, 14, []string{"alpha"}), "in use")

	// Unknown quotas can't be deleted
	must.ErrorContains(t, s.DeleteQuotaSpecs(structs.MsgTypeTestSetup, 15, []string{"nope"}), "not found")

	must.NoError(t, s.DeleteQuotaSpecs(structs.MsgTypeTestSetup, 16, []string{"beta"}))
	out, err = s.QuotaSpecByName(nil, "beta")
	must.NoError(t, err)
	must.Nil(t, out)
	usage, err = s.QuotaUsageByName(nil, "beta")
	must.NoError(t, err)
	must.Nil(t, usage)
}

func TestStateStore_QuotaUsage_Allocs(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)
	spec, ns, job := quotaTestSetup(t, s)

	// Allocations in other namespaces don't count
	other := mock.Alloc()
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 200, []*structs.Allocation{other}))
	must.True(t, quotaUsed(t, s, spec.Name).IsZero())

	a1 := quotaTestAlloc(job)
	a2 := quotaTestAlloc(job)
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 201, []*structs.Allocation{a1, a2}))
	must.Eq(t, structs.QuotaResources{CPU: 1000, MemoryMB: 512, MemoryMaxMB: 512, Allocations: 2},
		quotaUsed(t, s, spec.Name))

	// Updating an alloc without changing resources doesn't change usage
	a1u := a1.Copy()
	a1u.DesiredDescription = "updated"
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 202, []*structs.Allocation{a1u}))
	must.Eq(t, int64(2), quotaUsed(t, s, spec.Name).Allocations)

	// A client marking an alloc complete releases its usage
	a2c := a2.Copy()
	a2c.ClientStatus = structs.AllocClientStatusComplete
	must.NoError(t, s.UpdateAllocsFromClient(structs.MsgTypeTestSetup, 203, []*structs.Allocation{a2c}))
	must.Eq(t, structs.QuotaResources{CPU: 500, MemoryMB: 256, MemoryMaxMB: 256, Allocations: 1},
		quotaUsed(t, s, spec.Name))

	// Stopping via the scheduler releases it too
	a1s := a1.Copy()
	a1s.DesiredStatus = structs.AllocDesiredStatusStop
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 204, []*structs.Allocation{a1s}))
	must.True(t, quotaUsed(t, s, spec.Name).IsZero())

	// Deleting a non-terminal alloc releases its usage
	a3 := quotaTestAlloc(job)
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 205, []*structs.Allocation{a3}))
	must.Eq(t, int64(1), quotaUsed(t, s, spec.Name).Allocations)
	must.NoError(t, s.DeleteEval(206, nil, []string{a3.ID}, false))
	must.True(t, quotaUsed(t, s, spec.Name).IsZero())

	// Switching the namespace to another quota moves the usage
	a4 := quotaTestAlloc(job)
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 207, []*structs.Allocation{a4}))
	spec2 := testQuotaSpec("other")
	must.NoError(t, s.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 208, []*structs.QuotaSpec{spec2}))
	ns2 := ns.Copy()
	ns2.Quota = spec2.Name
	ns2.SetHash()
	must.NoError(t, s.UpsertNamespaces(209, []*structs.Namespace{ns2}))
	must.True(t, quotaUsed(t, s, spec.Name).IsZero())
	must.Eq(t, int64(1), quotaUsed(t, s, spec2.Name).Allocations)

	// Updating a spec recomputes its usage from scratch
	spec2u := spec2.Copy()
	spec2u.Limits[0].RegionLimit.CPU = 9999
	spec2u.SetHash()
	must.NoError(t, s.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 210, []*structs.QuotaSpec{spec2u}))
	usage, err := s.QuotaUsageByName(nil, spec2.Name)
	must.NoError(t, err)
	must.MapLen(t, 1, usage.Used)
	must.NotNil(t, usage.Used[spec2u.Limits[0].HashKey()])
	must.Eq(t, int64(1), quotaUsed(t, s, spec2.Name).Allocations)

	// Remaining capacity for the autoscaler
	rem, err := s.QuotaRemainingForNamespace(nil, ns.Name)
	must.NoError(t, err)
	must.Eq(t, spec2.Name, rem.Quota)
	must.Eq(t, int64(9999-500), *rem.CPU)
	must.Eq(t, int64(2), *rem.Allocations)
	must.Eq(t, 2, rem.MaxAllocs(structs.QuotaResources{CPU: 500, MemoryMB: 256, Allocations: 1}))

	rem, err = s.QuotaRemainingForNamespace(nil, structs.DefaultNamespace)
	must.NoError(t, err)
	must.Nil(t, rem)
}

func TestStateStore_QuotaUsage_OtherRegion(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)

	spec := testQuotaSpec("remote")
	spec.Limits[0].Region = "eu"
	spec.SetHash()
	must.NoError(t, s.UpsertQuotaSpecs(structs.MsgTypeTestSetup, 10, []*structs.QuotaSpec{spec}))

	ns := mock.Namespace()
	ns.Quota = spec.Name
	must.NoError(t, s.UpsertNamespaces(11, []*structs.Namespace{ns}))

	job := mock.Job()
	job.Namespace = ns.Name
	must.NoError(t, s.UpsertJob(structs.MsgTypeTestSetup, 12, nil, job))
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 13, []*structs.Allocation{quotaTestAlloc(job)}))

	// The quota doesn't limit the local region, so nothing is tracked
	usage, err := s.QuotaUsageByName(nil, spec.Name)
	must.NoError(t, err)
	must.MapEmpty(t, usage.Used)

	rem, err := s.QuotaRemainingForNamespace(nil, ns.Name)
	must.NoError(t, err)
	must.Nil(t, rem)
}

func TestStateStore_QuotaVariables(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)
	spec, ns, _ := quotaTestSetup(t, s)

	// 1 MiB is allowed
	sv := mock.VariableEncrypted()
	sv.Namespace = ns.Name
	sv.Data = make([]byte, 600*1024)
	resp := s.VarSet(300, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: sv})
	must.NoError(t, resp.Error)

	usage, err := s.QuotaUsageByName(nil, spec.Name)
	must.NoError(t, err)
	usage, err = s.QuotaUsageWithVariables(nil, usage)
	must.NoError(t, err)
	must.Eq(t, 1, *usage.RegionUsage("global").VariablesLimit)

	// A second variable that exceeds the limit is rejected
	sv2 := mock.VariableEncrypted()
	sv2.Namespace = ns.Name
	sv2.Data = make([]byte, 600*1024)
	resp = s.VarSet(301, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: sv2})
	must.ErrorContains(t, resp.Error, "exceeded")

	// Shrinking is allowed
	sv3 := sv.Copy()
	sv3.Data = make([]byte, 10)
	resp = s.VarSet(302, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: &sv3})
	must.NoError(t, resp.Error)
}

func TestStateStore_QuotaRestore(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)

	spec := testQuotaSpec("restored")
	usage := structs.NewQuotaUsage(spec, "global")
	usage.RegionUsage("global").SetUsageResources(structs.QuotaResources{CPU: 5, Allocations: 1})

	restore, err := s.Restore()
	must.NoError(t, err)
	must.NoError(t, restore.QuotaSpecRestore(spec))
	must.NoError(t, restore.QuotaUsageRestore(usage))
	must.NoError(t, restore.Commit())

	out, err := s.QuotaSpecByName(nil, spec.Name)
	must.NoError(t, err)
	must.Eq(t, spec, out)
	must.Eq(t, structs.QuotaResources{CPU: 5, Allocations: 1}, quotaUsed(t, s, spec.Name))
}

func TestEventsFromChanges_Quota(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	spec := testQuotaSpec("events")
	must.NoError(t, s.UpsertQuotaSpecs(structs.QuotaSpecUpsertRequestType, 100, []*structs.QuotaSpec{spec}))
	events := WaitForEvents(t, s, 100, 1, time.Second)
	must.Len(t, 1, events)
	must.Eq(t, structs.TopicQuota, events[0].Topic)
	must.Eq(t, structs.TypeQuotaSpecUpserted, events[0].Type)
	must.Eq(t, spec.Name, events[0].Key)

	ns := mock.Namespace()
	ns.Quota = spec.Name
	must.NoError(t, s.UpsertNamespaces(101, []*structs.Namespace{ns}))
	job := mock.Job()
	job.Namespace = ns.Name
	must.NoError(t, s.UpsertJob(structs.MsgTypeTestSetup, 102, nil, job))

	// Reaching the allocation limit of 3 exhausts the quota
	allocs := []*structs.Allocation{quotaTestAlloc(job), quotaTestAlloc(job), quotaTestAlloc(job)}
	must.NoError(t, s.UpsertAllocs(structs.AllocUpdateRequestType, 103, allocs))
	events = quotaEvents(WaitForEvents(t, s, 103, 1, time.Second))
	must.Len(t, 1, events)
	must.Eq(t, structs.TypeQuotaExhausted, events[0].Type)
	must.Eq(t, []string{structs.QuotaDimensionAllocations}, events[0].Payload.(*structs.QuotaUsageEvent).Dimensions)

	// Completing one frees it
	done := allocs[0].Copy()
	done.ClientStatus = structs.AllocClientStatusComplete
	must.NoError(t, s.UpdateAllocsFromClient(structs.AllocClientUpdateRequestType, 104, []*structs.Allocation{done}))
	events = quotaEvents(WaitForEvents(t, s, 104, 1, time.Second))
	must.Len(t, 1, events)
	must.Eq(t, structs.TypeQuotaFreed, events[0].Type)

	// Raising the limit while exhausted frees it in the same transaction
	must.NoError(t, s.UpsertAllocs(structs.AllocUpdateRequestType, 105, []*structs.Allocation{quotaTestAlloc(job)}))
	raised := spec.Copy()
	raised.Limits[0].Allocations = pointer.Of(10)
	raised.SetHash()
	must.NoError(t, s.UpsertQuotaSpecs(structs.QuotaSpecUpsertRequestType, 106, []*structs.QuotaSpec{raised}))
	events = quotaEvents(WaitForEvents(t, s, 106, 2, time.Second))
	must.Len(t, 2, events)
	must.Eq(t, structs.TypeQuotaSpecUpserted, events[0].Type)
	must.Eq(t, structs.TypeQuotaFreed, events[1].Type)
}

func quotaEvents(events []structs.Event) []structs.Event {
	var out []structs.Event
	for _, e := range events {
		if e.Topic == structs.TopicQuota {
			out = append(out, e)
		}
	}
	return out
}
