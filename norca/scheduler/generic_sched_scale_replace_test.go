// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func replProcess(t *testing.T, h *Harness, job *structs.Job, trigger string) *structs.Plan {
	t.Helper()
	eval := &structs.Evaluation{
		Namespace:    job.Namespace,
		ID:           uuid.Generate(),
		Priority:     job.Priority,
		TriggeredBy:  trigger,
		JobID:        job.ID,
		Status:       structs.EvalStatusPending,
		AnnotatePlan: true,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	before := len(h.Plans)
	must.NoError(t, h.Process(NewServiceScheduler, eval))
	if len(h.Plans) == before {
		return nil
	}
	return h.Plans[len(h.Plans)-1]
}

func replHealthy(t *testing.T, h *Harness, deploymentID string, ids ...string) {
	t.Helper()
	must.NoError(t, h.State.UpdateDeploymentAllocHealth(structs.MsgTypeTestSetup, h.NextIndex(),
		&structs.ApplyDeploymentAllocHealthRequest{
			DeploymentAllocHealthRequest: structs.DeploymentAllocHealthRequest{
				DeploymentID:         deploymentID,
				HealthyAllocationIDs: ids,
			},
			Timestamp: time.Now(),
		}))
}

func replDState(t *testing.T, h *Harness, id, group string) *structs.DeploymentState {
	t.Helper()
	d, err := h.State.DeploymentByID(nil, id)
	must.NoError(t, err)
	return d.TaskGroups[group]
}

// The replacement of an allocation that a scale up added to a group during
// its deployment (a migration or a lost node) stays outside the deployment
// like the allocation it replaces: it does not become a placement of the
// deployment, which would make the deployment succeed before its own last
// allocation was healthy (review finding SCH-1).
func TestServiceSched_ScaleAdded_ReplacementOutsideDeployment(t *testing.T) {
	ci.Parallel(t)
	for _, how := range []string{"migrate", "lost"} {
		t.Run(how, func(t *testing.T) {
			h, job, d := deployedHarness(t, structs.DeploymentStatusRunning, 2)
			_, err := h.State.ScaleJob(structs.MsgTypeTestSetup, h.NextIndex(), job.Namespace, job.ID, "web", 5, 0)
			must.NoError(t, err)
			must.NoError(t, h.Process(NewServiceScheduler, scalingEval(t, h, job)))
			added := placedAllocs(h.Plans[0])
			must.Len(t, 2, added)

			// Pick an added allocation alone on its node.
			all, err := h.State.AllocsByJob(nil, job.Namespace, job.ID, false)
			must.NoError(t, err)
			perNode := map[string]int{}
			for _, a := range all {
				perNode[a.NodeID]++
			}
			var victim *structs.Allocation
			for _, a := range added {
				if perNode[a.NodeID] == 1 {
					victim = a
				}
			}
			if victim == nil {
				t.Skip("no added allocation alone on its node")
			}
			must.Eq(t, "", victim.DeploymentID)

			switch how {
			case "migrate":
				must.NoError(t, h.State.UpdateNodeDrain(structs.MsgTypeTestSetup, h.NextIndex(), victim.NodeID,
					mock.DrainNode().DrainStrategy, false, time.Now().UnixNano(), nil, nil, ""))
				must.NoError(t, h.State.UpdateAllocsDesiredTransitions(structs.MsgTypeTestSetup, h.NextIndex(),
					map[string]*structs.DesiredTransition{victim.ID: {Migrate: pointer.Of(true)}}, nil))
			case "lost":
				must.NoError(t, h.State.UpdateNodeStatus(structs.MsgTypeTestSetup, h.NextIndex(), victim.NodeID,
					structs.NodeStatusDown, time.Now().UnixNano(), nil))
			}

			plan := replProcess(t, h, job, structs.EvalTriggerNodeUpdate)
			must.NotNil(t, plan)
			var repl *structs.Allocation
			for _, a := range placedAllocs(plan) {
				if a.PreviousAllocation == victim.ID {
					repl = a
				}
			}
			must.NotNil(t, repl)
			ds := replDState(t, h, d.ID, "web")
			t.Logf("replacement %s of added %s: DeploymentID=%q (running deployment %s)", repl.Name, victim.Name, repl.DeploymentID, d.ID)
			t.Logf("deployment after: desired=%d placed=%d healthy=%d scaleAdded=%d", ds.DesiredTotal, ds.PlacedAllocs, ds.HealthyAllocs, ds.ScaleAdded)
			must.Eq(t, "", repl.DeploymentID, must.Sprint("the replacement of an added allocation is outside the deployment"))

			// The deployment counts its own three allocations only.
			must.Eq(t, 3, ds.PlacedAllocs)
			must.Eq(t, 2, ds.HealthyAllocs)
			stored, err := h.State.DeploymentByID(nil, d.ID)
			must.NoError(t, err)
			must.NotEq(t, structs.DeploymentStatusSuccessful, stored.Status)
		})
	}
}

// Allocations that a scale up added during a rollout and that cannot be
// placed do not take the places of the deployment's max_parallel: the
// rollout goes on (review finding SCH-2).
func TestServiceSched_ScaleAdded_DoNotHoldTheRollout(t *testing.T) {
	ci.Parallel(t)
	for _, tc := range []struct {
		scale bool
		nodes int
	}{{false, 3}, {true, 3}, {true, 5}} {
		scale := tc.scale
		t.Run(fmt.Sprintf("scale=%v,nodes=%d", scale, tc.nodes), func(t *testing.T) {
			h := NewHarness(t)
			var nodes []*structs.Node
			for range tc.nodes {
				n := mock.Node()
				must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), n))
				nodes = append(nodes, n)
			}
			job := mock.Job()
			job.Constraints = append(job.Constraints, &structs.Constraint{Operand: structs.ConstraintDistinctHosts})
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
			var allocs []*structs.Allocation
			for i := range 3 {
				a := mock.Alloc()
				a.Job, a.JobID, a.NodeID = job, job.ID, nodes[i].ID
				a.Name = structs.AllocName(job.ID, "web", uint(i))
				a.ClientStatus = structs.AllocClientStatusRunning
				allocs = append(allocs, a)
			}
			must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

			// A destructive change: the rollout starts with one allocation.
			job2 := job.Copy()
			job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))
			job2, err = h.State.JobByID(nil, job.Namespace, job.ID)
			must.NoError(t, err)
			plan := replProcess(t, h, job2, structs.EvalTriggerJobRegister)
			must.NotNil(t, plan)
			must.NotNil(t, plan.Deployment)
			d := plan.Deployment
			first := placedAllocs(plan)
			must.Len(t, 1, first)
			replHealthy(t, h, d.ID, first[0].ID)

			if scale {
				_, err = h.State.ScaleJob(structs.MsgTypeTestSetup, h.NextIndex(), job.Namespace, job.ID, "web", 5, 0)
				must.NoError(t, err)
				t.Logf("ScaleAdded=%d", replDState(t, h, d.ID, "web").ScaleAdded)
			}

			// Each further evaluation of the deployment, as the watcher makes.
			for i := range 3 {
				plan = replProcess(t, h, job2, structs.EvalTriggerDeploymentWatcher)
				destructive, place := 0, 0
				if plan != nil && plan.Annotations != nil {
					u := plan.Annotations.DesiredTGUpdates["web"]
					destructive, place = int(u.DestructiveUpdate), int(u.Place)
				}
				last := h.Evals[len(h.Evals)-1]
				t.Logf("eval %d: plan=%v destructive=%d place=%d placed=%d failedTG=%v", i, plan != nil, destructive, place,
					func() int {
						if plan == nil {
							return 0
						}
						return len(placedAllocs(plan))
					}(), last.FailedTGAllocs != nil)
				if plan != nil {
					// let the new allocation of the step be healthy
					for _, a := range placedAllocs(plan) {
						if a.DeploymentID == d.ID {
							replHealthy(t, h, d.ID, a.ID)
						}
					}
				}
			}
			ds := replDState(t, h, d.ID, "web")
			t.Logf("deployment: desired=%d placed=%d healthy=%d", ds.DesiredTotal, ds.PlacedAllocs, ds.HealthyAllocs)
			// The rollout went on to all three allocations of the
			// deployment, whether the added ones could be placed or not.
			must.Eq(t, 3, ds.PlacedAllocs)
			must.Eq(t, 3, ds.HealthyAllocs)
		})
	}
}
