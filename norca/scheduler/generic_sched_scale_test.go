// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// scaledJobHarness has ten nodes and a service job with an update block and
// three running allocations of its version.
func scaledJobHarness(t *testing.T) (*Harness, *structs.Job, []*structs.Allocation) {
	h := NewHarness(t)
	var nodes []*structs.Node
	for range 10 {
		node := mock.Node()
		nodes = append(nodes, node)
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	job := mock.Job()
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Update = &structs.UpdateStrategy{
		MaxParallel:      1,
		HealthCheck:      structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:   10 * time.Second,
		HealthyDeadline:  5 * time.Minute,
		ProgressDeadline: 10 * time.Minute,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	job, err := h.State.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)

	var allocs []*structs.Allocation
	for i := range 3 {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = structs.AllocName(job.ID, "web", uint(i))
		alloc.ClientStatus = structs.AllocClientStatusRunning
		allocs = append(allocs, alloc)
	}
	return h, job, allocs
}

func scalingEval(t *testing.T, h *Harness, job *structs.Job) *structs.Evaluation {
	eval := &structs.Evaluation{
		Namespace:   job.Namespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerScaling,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	return eval
}

func placedAllocs(plan *structs.Plan) []*structs.Allocation {
	var placed []*structs.Allocation
	for _, list := range plan.NodeAllocation {
		placed = append(placed, list...)
	}
	return placed
}

// Scaling sets the count on the version of the job that runs (state.ScaleJob):
// the allocations it adds are placed without a deployment, and those that run
// are left as they are.
func TestServiceSched_Scale_NoDeployment(t *testing.T) {
	ci.Parallel(t)

	h, job, allocs := scaledJobHarness(t)
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	_, err := h.State.ScaleJob(structs.MsgTypeTestSetup, h.NextIndex(), job.Namespace, job.ID, "web", 5, 0)
	must.NoError(t, err)
	must.NoError(t, h.Process(NewServiceScheduler, scalingEval(t, h, job)))

	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]
	must.Nil(t, plan.Deployment)
	must.Len(t, 0, plan.DeploymentUpdates)
	must.MapEmpty(t, plan.NodeUpdate)

	placed := placedAllocs(plan)
	must.Len(t, 2, placed)
	for _, alloc := range placed {
		must.Eq(t, "", alloc.DeploymentID)
	}
	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

// deployedHarness is scaledJobHarness with a deployment of the job's version
// over its three allocations, two of them healthy, in the given status.
func deployedHarness(t *testing.T, status string, healthy int) (*Harness, *structs.Job, *structs.Deployment) {
	h, job, allocs := scaledJobHarness(t)
	d := structs.NewDeployment(job, 50)
	d.Status = status
	d.TaskGroups["web"] = &structs.DeploymentState{
		DesiredTotal:     3,
		HealthyAllocs:    healthy, // the state store counts the placed ones
		ProgressDeadline: 10 * time.Minute, RequireProgressBy: time.Now().Add(10 * time.Minute),
	}
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d))
	for i, alloc := range allocs {
		alloc.DeploymentID = d.ID
		if i < healthy {
			alloc.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: new(bool)}
			*alloc.DeploymentStatus.Healthy = true
		}
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))
	return h, job, d
}

// A deployment of the version that runs is not cancelled by a scale up. It
// goes on as it was: the allocations that are added are placed without it.
func TestServiceSched_Scale_KeepsTheDeployment(t *testing.T) {
	ci.Parallel(t)

	h, job, d := deployedHarness(t, structs.DeploymentStatusRunning, 2)
	_, err := h.State.ScaleJob(structs.MsgTypeTestSetup, h.NextIndex(), job.Namespace, job.ID, "web", 5, 0)
	must.NoError(t, err)
	must.NoError(t, h.Process(NewServiceScheduler, scalingEval(t, h, job)))

	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]
	must.Nil(t, plan.Deployment)
	must.Len(t, 0, plan.DeploymentUpdates)
	must.MapEmpty(t, plan.NodeUpdate)

	placed := placedAllocs(plan)
	must.Len(t, 2, placed)
	for _, alloc := range placed {
		must.Eq(t, "", alloc.DeploymentID)
		must.True(t, alloc.Index() >= 3)
	}

	stored, err := h.State.DeploymentByID(nil, d.ID)
	must.NoError(t, err)
	must.Eq(t, structs.DeploymentStatusRunning, stored.Status)
	must.Eq(t, 3, stored.TaskGroups["web"].DesiredTotal)
	must.Eq(t, 2, stored.TaskGroups["web"].ScaleAdded)
	must.Eq(t, 3, stored.TaskGroups["web"].PlacedAllocs)
	must.Eq(t, 2, stored.TaskGroups["web"].HealthyAllocs)
}

// The deployment does not wait for the allocations a scale up added: it is
// complete when its own are healthy, also while an added one can not be
// placed.
func TestServiceSched_Scale_DeploymentCompletesWithoutTheAdded(t *testing.T) {
	ci.Parallel(t)

	h, job, d := deployedHarness(t, structs.DeploymentStatusRunning, 3)

	// No node has room for more: the two that are added wait for capacity.
	nodes, err := h.State.Nodes(nil)
	must.NoError(t, err)
	for raw := nodes.Next(); raw != nil; raw = nodes.Next() {
		must.NoError(t, h.State.UpdateNodeEligibility(structs.MsgTypeTestSetup, h.NextIndex(), raw.(*structs.Node).ID,
			structs.NodeSchedulingIneligible, 0, nil))
	}

	_, err = h.State.ScaleJob(structs.MsgTypeTestSetup, h.NextIndex(), job.Namespace, job.ID, "web", 5, 0)
	must.NoError(t, err)
	must.NoError(t, h.Process(NewServiceScheduler, scalingEval(t, h, job)))

	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]
	must.Len(t, 0, placedAllocs(plan))
	must.Len(t, 1, plan.DeploymentUpdates)
	must.Eq(t, d.ID, plan.DeploymentUpdates[0].DeploymentID)
	must.Eq(t, structs.DeploymentStatusSuccessful, plan.DeploymentUpdates[0].Status)

	// The two wait as blocked work does.
	must.Len(t, 1, h.CreateEvals)
	must.Eq(t, structs.EvalStatusBlocked, h.CreateEvals[0].Status)
}

// A deployment that failed does not hold the allocations a scale up adds;
// one that is paused holds them like every placement.
func TestServiceSched_Scale_FailedAndPausedDeployment(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		status   string
		expected int
	}{
		{name: "failed", status: structs.DeploymentStatusFailed, expected: 2},
		{name: "paused", status: structs.DeploymentStatusPaused, expected: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, job, _ := deployedHarness(t, tt.status, 2)
			_, err := h.State.ScaleJob(structs.MsgTypeTestSetup, h.NextIndex(), job.Namespace, job.ID, "web", 5, 0)
			must.NoError(t, err)
			must.NoError(t, h.Process(NewServiceScheduler, scalingEval(t, h, job)))

			var placed []*structs.Allocation
			for _, plan := range h.Plans {
				must.Nil(t, plan.Deployment)
				must.Len(t, 0, plan.DeploymentUpdates)
				placed = append(placed, placedAllocs(plan)...)
			}
			must.Len(t, tt.expected, placed)
			for _, alloc := range placed {
				must.Eq(t, "", alloc.DeploymentID)
			}
		})
	}
}

// A replacement keeps the place of the allocation it replaces in the
// deployment, with a scale up at the same time too.
func TestMarkOutsideDeployment(t *testing.T) {
	ci.Parallel(t)

	tg := &structs.TaskGroup{Name: "web"}
	name := func(i uint) string { return structs.AllocName("shop", "web", i) }
	place := []allocPlaceResult{
		{name: name(1), taskGroup: tg, previousAlloc: &structs.Allocation{}, reschedule: true},
		{name: name(4), taskGroup: tg, previousAlloc: &structs.Allocation{}, lost: true},
		{name: name(2), taskGroup: tg},
		{name: name(3), taskGroup: tg},
		{name: name(4), taskGroup: tg},
		{name: name(5), taskGroup: tg, canary: true},
	}
	markOutsideDeployment("shop", place, 3)

	var outside []int
	for i, p := range place {
		if p.OutsideDeployment() {
			outside = append(outside, i)
		}
	}
	must.Eq(t, []int{3, 4}, outside)
	must.Eq(t, 4, insideDeployment(place))
	must.Len(t, 2, outsideDeployment(place, 0))
	must.Len(t, 1, outsideDeployment(place, 4))
	must.Eq(t, "", placementDeploymentID(place[3], "d1"))
	must.Eq(t, "d1", placementDeploymentID(place[2], "d1"))
}

// Registering the job again with another count is a new version of it, as it
// always was: the allocations are updated to it under a deployment.
func TestServiceSched_Register_CountChangeIsAVersion(t *testing.T) {
	ci.Parallel(t)

	h, job, allocs := scaledJobHarness(t)
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	updated := job.Copy()
	updated.TaskGroups[0].Count = 5
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, updated))
	must.NoError(t, h.Process(NewServiceScheduler, scalingEval(t, h, job)))

	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]
	must.NotNil(t, plan.Deployment)
	must.Eq(t, uint64(1), plan.Deployment.JobVersion)
	must.Eq(t, 5, plan.Deployment.TaskGroups["web"].DesiredTotal)
}
