// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package drainer

import (
	"testing"
	"time"

	"github.com/shoenig/test"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TestHandleTaskGroup_Disruption tests that the drainer applies the stricter
// of migrate.max_parallel and the disruption budget.
func TestHandleTaskGroup_Disruption(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		name        string
		maxParallel int
		budget      *structs.DisruptionBudget
		update      *structs.UpdateStrategy
		deployment  *structs.DeploymentState

		// addAllocFn is called for each of the 6 allocs, which default to
		// running and healthy on the draining node
		addAllocFn func(idx int, a *structs.Allocation, drainingID, runningID string)

		expectDrained int
	}{
		{
			name:          "budget-stricter-than-max-parallel",
			maxParallel:   6,
			budget:        &structs.DisruptionBudget{MinHealthy: "4"},
			expectDrained: 2,
		},
		{
			name:          "max-parallel-stricter-than-budget",
			maxParallel:   1,
			budget:        &structs.DisruptionBudget{MinHealthy: "4"},
			expectDrained: 1,
		},
		{
			name:          "max-unavailable-percent",
			maxParallel:   6,
			budget:        &structs.DisruptionBudget{MaxUnavailable: "50%"},
			expectDrained: 3,
		},
		{
			name:          "budget-blocks-everything",
			maxParallel:   6,
			budget:        &structs.DisruptionBudget{MinHealthy: "100%"},
			expectDrained: 0,
		},
		{
			name:        "pending-migration-counts-as-unavailable",
			maxParallel: 6,
			budget:      &structs.DisruptionBudget{MinHealthy: "4"},
			addAllocFn: func(i int, a *structs.Allocation, _, _ string) {
				if i == 0 {
					a.DesiredTransition.Migrate = pointer.Of(true)
				}
			},
			expectDrained: 1,
		},
		{
			name:        "unhealthy-allocs-drain-first",
			maxParallel: 6,
			budget:      &structs.DisruptionBudget{MinHealthy: "4"},
			addAllocFn: func(i int, a *structs.Allocation, _, runningID string) {
				switch i {
				case 0:
					// unhealthy on the draining node
					a.DeploymentStatus.Healthy = pointer.Of(false)
				case 1:
					a.NodeID = runningID
				}
			},
			// 4 healthy allocs on the draining node, 5 healthy in total:
			// the unhealthy one and one healthy one
			expectDrained: 2,
		},
		{
			name:        "no-unhealthy-drain-below-budget",
			maxParallel: 6,
			budget:      &structs.DisruptionBudget{MinHealthy: "5"},
			addAllocFn: func(i int, a *structs.Allocation, _, _ string) {
				if i < 2 {
					a.DeploymentStatus.Healthy = pointer.Of(false)
				}
			},
			expectDrained: 0,
		},
		{
			name:          "active-deployment-reserves-next-step",
			maxParallel:   6,
			budget:        &structs.DisruptionBudget{MinHealthy: "3"},
			update:        &structs.UpdateStrategy{MaxParallel: 2},
			deployment:    &structs.DeploymentState{DesiredTotal: 6, PlacedAllocs: 2, HealthyAllocs: 2},
			expectDrained: 1,
		},
		{
			name:          "placed-deployment-reserves-nothing",
			maxParallel:   6,
			budget:        &structs.DisruptionBudget{MinHealthy: "3"},
			update:        &structs.UpdateStrategy{MaxParallel: 2},
			deployment:    &structs.DeploymentState{DesiredTotal: 6, PlacedAllocs: 6, HealthyAllocs: 6},
			expectDrained: 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)

			store := state.TestStateStore(t)
			drainingNode, runningNode := testNodes(t, store)

			job := mock.Job()
			tg := job.TaskGroups[0]
			tg.Count = 6
			tg.Migrate.MaxParallel = tc.maxParallel
			tg.Disruption = tc.budget
			tg.Update = tc.update
			must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 102, nil, job))

			if tc.deployment != nil {
				d := mock.Deployment()
				d.JobID = job.ID
				d.TaskGroups = map[string]*structs.DeploymentState{tg.Name: tc.deployment}
				must.NoError(t, store.UpsertDeployment(103, d))
			}

			var allocs []*structs.Allocation
			for i := 0; i < 6; i++ {
				a := mock.Alloc()
				a.JobID = job.ID
				a.Job = job
				a.TaskGroup = tg.Name
				a.NodeID = drainingNode.ID
				a.ClientStatus = structs.AllocClientStatusRunning
				a.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
				if tc.addAllocFn != nil {
					tc.addAllocFn(i, a, drainingNode.ID, runningNode.ID)
				}
				allocs = append(allocs, a)
			}
			must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 104, allocs))

			snap, err := store.Snapshot()
			must.NoError(t, err)

			res := newJobResult()
			must.NoError(t, handleTaskGroup(snap, false, tg, allocs, 102, res))
			test.Len(t, tc.expectDrained, res.drain, test.Sprint("expected drained allocs"))
			for _, a := range res.drain {
				test.False(t, a.DesiredTransition.ShouldMigrate())
			}
		})
	}
}

// TestDrainingJobWatcher_DisruptionBudget_MultipleJobs drains three nodes at
// once, shared by two jobs whose 6 allocations are spread over all of them,
// and asserts that a group with min_healthy = 4 never has fewer than 4
// healthy allocations.
func TestDrainingJobWatcher_DisruptionBudget_MultipleJobs(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	jobWatcher, cancelWatcher := testDrainingJobWatcher(t, store)
	defer cancelWatcher()

	var index uint64 = 100
	var drainingNodes []*structs.Node
	for i := 0; i < 3; i++ {
		n := mock.Node()
		n.DrainStrategy = &structs.DrainStrategy{
			DrainSpec:     structs.DrainSpec{Deadline: time.Hour},
			ForceDeadline: time.Now().Add(time.Hour),
		}
		must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, index, n))
		index++
		drainingNodes = append(drainingNodes, n)
	}
	runningNode := mock.Node()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, index, runningNode))
	index++

	newAlloc := func(node *structs.Node, job *structs.Job, healthy bool) *structs.Allocation {
		a := mock.Alloc()
		a.JobID = job.ID
		a.Job = job
		a.TaskGroup = job.TaskGroups[0].Name
		a.NodeID = node.ID
		if healthy {
			a.ClientStatus = structs.AllocClientStatusRunning
			a.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
		}
		return a
	}

	jobs := make([]*structs.Job, 2)
	jnss := make([]structs.NamespacedID, 2)
	for i := range jobs {
		job := mock.Job()
		job.TaskGroups[0].Count = 6
		// max_parallel alone would allow draining everything at once
		job.TaskGroups[0].Migrate.MaxParallel = 6
		job.TaskGroups[0].Disruption = &structs.DisruptionBudget{MinHealthy: "4"}
		must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))
		index++
		jobs[i] = job
		jnss[i] = structs.NamespacedID{Namespace: job.Namespace, ID: job.ID}

		var allocs []*structs.Allocation
		for j := 0; j < 6; j++ {
			allocs = append(allocs, newAlloc(drainingNodes[j%3], job, true))
		}
		must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index, allocs))
		index++
	}

	assertBudget := func() {
		t.Helper()
		for _, job := range jobs {
			allocs, err := store.AllocsByJob(nil, job.Namespace, job.ID, false)
			must.NoError(t, err)
			healthy := 0
			for _, a := range allocs {
				if a.HealthyForDisruption() {
					healthy++
				}
			}
			must.GreaterEq(t, 4, healthy, must.Sprintf("job %s", job.ID))
		}
	}

	jobWatcher.RegisterJobs(jnss)

	// 6 allocations per job need 3 rounds of 2 migrations each
	for round := 0; round < 3; round++ {
		drains, _ := assertJobWatcherOps(t, jobWatcher, 4, 0)
		perJob := map[string]int{}
		for _, a := range drains.Allocs {
			perJob[a.JobID]++
		}
		for _, job := range jobs {
			must.Eq(t, 2, perJob[job.ID], must.Sprintf("round %d job %s", round, job.ID))
		}

		// Mark the drained allocs for migration, as the drainer does
		drained := make([]*structs.Allocation, len(drains.Allocs))
		for i, a := range drains.Allocs {
			a = a.Copy()
			a.DesiredTransition.Migrate = pointer.Of(true)
			drained[i] = a
		}
		must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index, drained))
		drains.Resp.Respond(index, nil)
		index++
		assertBudget()

		// The budget is exhausted until the replacements are healthy
		assertJobWatcherOps(t, jobWatcher, 0, 0)

		// The scheduler stops the drained allocs and places replacements
		updates := make([]*structs.Allocation, 0, len(drained)*2)
		replacements := make([]*structs.Allocation, 0, len(drained))
		for _, a := range drained {
			a = a.Copy()
			a.DesiredTransition.Migrate = nil
			a.DesiredStatus = structs.AllocDesiredStatusStop
			r := newAlloc(runningNode, a.Job, false)
			updates = append(updates, a, r)
			replacements = append(replacements, r.Copy())
		}
		must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index, updates))
		index++
		assertBudget()
		assertJobWatcherOps(t, jobWatcher, 0, 0)

		// The client stops the drained allocs
		complete := make([]*structs.Allocation, len(drained))
		for i, a := range drained {
			a = a.Copy()
			a.ClientStatus = structs.AllocClientStatusComplete
			complete[i] = a
		}
		must.NoError(t, store.UpdateAllocsFromClient(structs.MsgTypeTestSetup, index, complete))
		index++
		assertBudget()
		assertJobWatcherOps(t, jobWatcher, 0, 4)

		// The replacements become healthy, which allows the next round
		for _, r := range replacements {
			r.ClientStatus = structs.AllocClientStatusRunning
			r.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
		}
		must.NoError(t, store.UpdateAllocsFromClient(structs.MsgTypeTestSetup, index, replacements))
		index++
		assertBudget()
	}

	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return len(jobWatcher.drainingJobs()) == 0 }),
		wait.Timeout(5*time.Second),
	), must.Sprint("expected all jobs to finish draining"))
}

// TestDrainingJobWatcher_DisruptionBudget_JobUpdate asserts that relaxing the
// budget of a job wakes up the drainer even without allocation changes.
func TestDrainingJobWatcher_DisruptionBudget_JobUpdate(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	jobWatcher, cancelWatcher := testDrainingJobWatcher(t, store)
	defer cancelWatcher()
	drainingNode, _ := testNodes(t, store)

	job := mock.Job()
	job.TaskGroups[0].Count = 4
	job.TaskGroups[0].Migrate.MaxParallel = 4
	job.TaskGroups[0].Disruption = &structs.DisruptionBudget{MinHealthy: "4"}
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 110, nil, job))

	var allocs []*structs.Allocation
	for i := 0; i < 4; i++ {
		a := mock.Alloc()
		a.JobID = job.ID
		a.Job = job
		a.TaskGroup = job.TaskGroups[0].Name
		a.NodeID = drainingNode.ID
		a.ClientStatus = structs.AllocClientStatusRunning
		a.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
		allocs = append(allocs, a)
	}
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 111, allocs))

	jobWatcher.RegisterJobs([]structs.NamespacedID{{Namespace: job.Namespace, ID: job.ID}})
	assertJobWatcherOps(t, jobWatcher, 0, 0)

	job2 := job.Copy()
	job2.TaskGroups[0].Disruption = &structs.DisruptionBudget{MaxUnavailable: "1"}
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 112, nil, job2))

	drains, _ := assertJobWatcherOps(t, jobWatcher, 1, 0)
	drains.Resp.Respond(112, nil)
}
