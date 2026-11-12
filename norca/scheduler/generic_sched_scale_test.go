// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"fmt"
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
		alloc.Name = fmt.Sprintf("my-job.web[%d]", i)
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

// A deployment of the version that runs is not cancelled by a scale up: it
// goes on, and the allocations that are added are part of it.
func TestServiceSched_Scale_KeepsTheDeployment(t *testing.T) {
	ci.Parallel(t)

	h, job, allocs := scaledJobHarness(t)
	d := structs.NewDeployment(job, 50)
	d.TaskGroups["web"] = &structs.DeploymentState{
		DesiredTotal: 3, PlacedAllocs: 3, HealthyAllocs: 2,
		ProgressDeadline: 10 * time.Minute, RequireProgressBy: time.Now().Add(10 * time.Minute),
	}
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d))
	for i, alloc := range allocs {
		alloc.DeploymentID = d.ID
		if i < 2 {
			alloc.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: new(bool)}
			*alloc.DeploymentStatus.Healthy = true
		}
	}
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
		must.Eq(t, d.ID, alloc.DeploymentID)
	}

	stored, err := h.State.DeploymentByID(nil, d.ID)
	must.NoError(t, err)
	must.Eq(t, structs.DeploymentStatusRunning, stored.Status)
	must.Eq(t, 5, stored.TaskGroups["web"].DesiredTotal)
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
