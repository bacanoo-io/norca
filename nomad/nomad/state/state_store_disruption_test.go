// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func disruptionTestJob(t *testing.T, s *StateStore, index uint64, count int, budget *structs.DisruptionBudget) (*structs.Job, []*structs.Allocation) {
	t.Helper()
	job := mock.Job()
	job.TaskGroups[0].Count = count
	job.TaskGroups[0].Disruption = budget
	must.NoError(t, s.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))

	allocs := make([]*structs.Allocation, count)
	for i := range allocs {
		a := mock.Alloc()
		a.Job = job
		a.JobID = job.ID
		a.TaskGroup = job.TaskGroups[0].Name
		a.ClientStatus = structs.AllocClientStatusRunning
		a.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
		allocs[i] = a
	}
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, index+1, allocs))
	return job, allocs
}

func TestStateStore_EvictAlloc(t *testing.T) {
	ci.Parallel(t)

	s := testStateStore(t)
	job, allocs := disruptionTestJob(t, s, 100, 4, &structs.DisruptionBudget{MaxUnavailable: "1"})

	evict := func(index uint64, alloc *structs.Allocation) (bool, error) {
		eval := mock.Eval()
		eval.JobID = job.ID
		return s.EvictAlloc(structs.AllocEvictRequestType, index, &structs.AllocEvictRequest{
			AllocID: alloc.ID, Eval: eval, NoShutdownDelay: true,
		})
	}

	// The first eviction is allowed
	evicted, err := evict(200, allocs[0])
	must.NoError(t, err)
	must.True(t, evicted)

	out, err := s.AllocByID(nil, allocs[0].ID)
	must.NoError(t, err)
	must.True(t, out.DesiredTransition.ShouldMigrate())
	must.True(t, out.DesiredTransition.ShouldIgnoreShutdownDelay())
	must.Eq(t, 200, out.ModifyIndex)
	evals, err := s.EvalsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 1, evals)

	// Evicting the same allocation again is a no-op
	evicted, err = evict(201, allocs[0])
	must.NoError(t, err)
	must.False(t, evicted)

	// The second one exceeds the budget while the first is pending
	evicted, err = evict(202, allocs[1])
	must.True(t, errors.Is(err, structs.ErrDisruptionBudgetExceeded))
	must.False(t, evicted)
	out, err = s.AllocByID(nil, allocs[1].ID)
	must.NoError(t, err)
	must.False(t, out.DesiredTransition.ShouldMigrate())
	evals, err = s.EvalsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 1, evals)

	// An unhealthy allocation can be evicted while the group meets its
	// requirement: 3 healthy allocations, 3 required
	unhealthy := allocs[2].Copy()
	unhealthy.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(false)}
	must.NoError(t, s.UpdateAllocsFromClient(structs.MsgTypeTestSetup, 203, []*structs.Allocation{unhealthy}))
	_, err = evict(204, allocs[1])
	must.True(t, errors.Is(err, structs.ErrDisruptionBudgetExceeded))
	// ...but not after that, as the group is below the requirement
	evicted, err = evict(205, allocs[2])
	must.True(t, errors.Is(err, structs.ErrDisruptionBudgetExceeded))
	must.False(t, evicted)

	// Unknown allocations
	_, err = s.EvictAlloc(structs.AllocEvictRequestType, 206, &structs.AllocEvictRequest{AllocID: uuid.Generate()})
	must.True(t, structs.IsErrUnknownAllocation(err))
}

func TestStateStore_EvictAlloc_NoBudget(t *testing.T) {
	ci.Parallel(t)

	s := testStateStore(t)
	_, allocs := disruptionTestJob(t, s, 100, 2, nil)

	for i, alloc := range allocs {
		evicted, err := s.EvictAlloc(structs.AllocEvictRequestType, uint64(200+i), &structs.AllocEvictRequest{AllocID: alloc.ID})
		must.NoError(t, err)
		must.True(t, evicted)
	}
}

func TestStateStore_DisruptionStatusByJob(t *testing.T) {
	ci.Parallel(t)

	s := testStateStore(t)
	job, allocs := disruptionTestJob(t, s, 100, 6, &structs.DisruptionBudget{MinHealthy: "50%"})

	stopped := allocs[0].Copy()
	stopped.DesiredStatus = structs.AllocDesiredStatusStop
	must.NoError(t, s.UpsertAllocs(structs.MsgTypeTestSetup, 110, []*structs.Allocation{stopped}))

	out, index, err := s.DisruptionStatusByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, 110, index)
	must.MapLen(t, 1, out)
	status := out[job.TaskGroups[0].Name]
	must.Eq(t, 5, status.Healthy)
	must.Eq(t, 3, status.RequiredHealthy)
	must.Eq(t, 2, status.DisruptionsAllowed)

	out, _, err = s.DisruptionStatusByJob(nil, job.Namespace, "missing")
	must.NoError(t, err)
	must.Nil(t, out)
}

func TestEventsFromChanges_AllocEvict(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	job, allocs := disruptionTestJob(t, s, 10, 2, &structs.DisruptionBudget{MinHealthy: "1"})
	eval := mock.Eval()
	eval.JobID = job.ID

	evicted, err := s.EvictAlloc(structs.AllocEvictRequestType, 100,
		&structs.AllocEvictRequest{AllocID: allocs[0].ID, Eval: eval})
	must.NoError(t, err)
	must.True(t, evicted)

	events := WaitForEvents(t, s, 100, 1, 1*time.Second)
	must.Len(t, 2, events)
	topics := map[structs.Topic]bool{}
	for _, e := range events {
		must.Eq(t, structs.TypeAllocationEvicted, e.Type)
		topics[e.Topic] = true
	}
	must.True(t, topics[structs.TopicAllocation])
	must.True(t, topics[structs.TopicEvaluation])
}
