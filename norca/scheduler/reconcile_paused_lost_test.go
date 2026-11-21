// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// A paused deployment, one allocation to reschedule now and one lost in the
// same evaluation: each gets one replacement (review finding SCH-3: the
// failed one got two, with the same name, and the lost one none).
func TestServiceSched_PausedDeployment_RescheduleAndLost(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	var nodes []*structs.Node
	for range 5 {
		n := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), n))
		nodes = append(nodes, n)
	}
	job := mock.Job()
	tg := job.TaskGroups[0]
	tg.Count = 3
	tg.Update = &structs.UpdateStrategy{
		MaxParallel:      1,
		HealthCheck:      structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:   10 * time.Second,
		HealthyDeadline:  5 * time.Minute,
		ProgressDeadline: 10 * time.Minute,
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	job, err := h.State.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)

	d := structs.NewDeployment(job, 50)
	d.Status = structs.DeploymentStatusPaused
	d.StatusDescription = structs.DeploymentStatusDescriptionPaused
	d.TaskGroups["web"] = &structs.DeploymentState{
		DesiredTotal: 3, HealthyAllocs: 2, UnhealthyAllocs: 1,
		ProgressDeadline: 10 * time.Minute, RequireProgressBy: time.Now().Add(10 * time.Minute),
	}
	must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d))

	var allocs []*structs.Allocation
	for i := range 3 {
		a := mock.Alloc()
		a.Job, a.JobID, a.NodeID = job, job.ID, nodes[i].ID
		a.Name = structs.AllocName(job.ID, "web", uint(i))
		a.ClientStatus = structs.AllocClientStatusRunning
		a.DeploymentID = d.ID
		a.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
		allocs = append(allocs, a)
	}
	// web[1] failed; the deployment watcher allowed its replacement.
	failed := allocs[1]
	failed.ClientStatus = structs.AllocClientStatusFailed
	failed.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(false)}
	failed.DesiredTransition.Reschedule = pointer.Of(true)
	failed.TaskStates = map[string]*structs.TaskState{"web": {
		State: structs.TaskStateDead, Failed: true,
		StartedAt:  time.Now().Add(-time.Hour),
		FinishedAt: time.Now().Add(-10 * time.Second),
	}}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))
	// web[2]'s node goes down.
	lost := allocs[2]
	must.NoError(t, h.State.UpdateNodeStatus(structs.MsgTypeTestSetup, h.NextIndex(), lost.NodeID,
		structs.NodeStatusDown, time.Now().UnixNano(), nil))

	plan := replProcess(t, h, job, structs.EvalTriggerNodeUpdate)
	must.NotNil(t, plan)
	names := map[string]string{failed.ID: "failed web[1]", lost.ID: "lost web[2]"}
	byPrev := map[string]int{}
	for _, a := range placedAllocs(plan) {
		byPrev[names[a.PreviousAllocation]]++
	}
	// One replacement each: the failed web[1] and the lost web[2].
	must.Eq(t, 1, byPrev["failed web[1]"])
	must.Eq(t, 1, byPrev["lost web[2]"])
}
