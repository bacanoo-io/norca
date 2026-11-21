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

// Two groups, both with an unpromoted canary of v1 under d1, and a node lost
// that holds a v0 non-canary of a and the canary of b. The replacement of a is
// downgraded to v0 and its deployment d0; the deployment of the placements
// after it in the same plan stays d1 (review finding SCH-5: the new canary of
// b took d0, was missing from d1's canaries, and d1 was stuck).
func TestServiceSched_DowngradedPlacementKeepsItsDeployment(t *testing.T) {
	ci.Parallel(t)

	// The order of the groups in a plan comes from a map: run it often
	// enough to see both orders.
	for run := range 16 {
		h := NewHarness(t)
		var nodes []*structs.Node
		for range 4 {
			n := mock.Node()
			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), n))
			nodes = append(nodes, n)
		}
		job := mock.Job()
		a := job.TaskGroups[0]
		a.Name = "a"
		a.Count = 2
		a.Update = &structs.UpdateStrategy{
			MaxParallel: 1, Canary: 1,
			HealthCheck:      structs.UpdateStrategyHealthCheck_Checks,
			MinHealthyTime:   10 * time.Second,
			HealthyDeadline:  5 * time.Minute,
			ProgressDeadline: 10 * time.Minute,
		}
		b := a.Copy()
		b.Name = "b"
		job.TaskGroups = []*structs.TaskGroup{a, b}
		must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
		job0, err := h.State.JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)

		d0 := structs.NewDeployment(job0, 50)
		d0.Status = structs.DeploymentStatusSuccessful
		for _, g := range []string{"a", "b"} {
			d0.TaskGroups[g] = &structs.DeploymentState{DesiredTotal: 2, PlacedAllocs: 2, HealthyAllocs: 2}
		}
		must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d0))

		job1 := job0.Copy()
		for _, g := range job1.TaskGroups {
			g.Tasks[0].Config["command"] = "/bin/other"
		}
		must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job1))
		job1, err = h.State.JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		must.Eq(t, uint64(1), job1.Version)

		mk := func(j *structs.Job, g string, idx uint, node *structs.Node, dID string, canary bool) *structs.Allocation {
			al := mock.Alloc()
			al.Job, al.JobID, al.TaskGroup, al.NodeID = j, j.ID, g, node.ID
			al.Name = structs.AllocName(j.ID, g, idx)
			al.ClientStatus = structs.AllocClientStatusRunning
			al.DeploymentID = dID
			al.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true), Canary: canary}
			// no reserved ports: two of these share a node
			al.Resources = nil
			al.TaskResources = nil
			al.SharedResources = nil
			for _, tr := range al.AllocatedResources.Tasks {
				tr.Networks = nil
			}
			al.AllocatedResources.Shared.Networks = nil
			al.AllocatedResources.Shared.Ports = nil
			return al
		}
		// node 0 holds a non-canary of a (v0) and the canary of b (v1).
		a0 := mk(job0, "a", 0, nodes[0], d0.ID, false)
		a1 := mk(job0, "a", 1, nodes[1], d0.ID, false)
		b0 := mk(job0, "b", 0, nodes[1], d0.ID, false)
		b1 := mk(job0, "b", 1, nodes[2], d0.ID, false)
		d1 := structs.NewDeployment(job1, 50)
		canA := mk(job1, "a", 0, nodes[3], d1.ID, true)
		canB := mk(job1, "b", 0, nodes[0], d1.ID, true)
		d1.TaskGroups["a"] = &structs.DeploymentState{DesiredTotal: 2, DesiredCanaries: 1, PlacedCanaries: []string{canA.ID}, PlacedAllocs: 1,
			ProgressDeadline: 10 * time.Minute, RequireProgressBy: time.Now().Add(10 * time.Minute)}
		d1.TaskGroups["b"] = &structs.DeploymentState{DesiredTotal: 2, DesiredCanaries: 1, PlacedCanaries: []string{canB.ID}, PlacedAllocs: 1,
			ProgressDeadline: 10 * time.Minute, RequireProgressBy: time.Now().Add(10 * time.Minute)}
		must.NoError(t, h.State.UpsertDeployment(h.NextIndex(), d1))
		must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{a0, a1, b0, b1, canA, canB}))

		must.NoError(t, h.State.UpdateNodeStatus(structs.MsgTypeTestSetup, h.NextIndex(), nodes[0].ID,
			structs.NodeStatusDown, time.Now().UnixNano(), nil))

		plan := replProcess(t, h, job1, structs.EvalTriggerNodeUpdate)
		must.NotNil(t, plan)
		var aRepl, bCanary *structs.Allocation
		for _, al := range placedAllocs(plan) {
			switch {
			case al.TaskGroup == "a" && al.PreviousAllocation == a0.ID:
				aRepl = al
			case al.TaskGroup == "b" && al.DeploymentStatus.IsCanary():
				bCanary = al
			}
		}
		must.NotNil(t, aRepl)
		must.NotNil(t, bCanary)
		// The lost non-canary of a is placed with v0, under d0; the new
		// canary of b, placed after it in the same plan, is d1's.
		must.Eq(t, d0.ID, aRepl.DeploymentID, must.Sprintf("run %d", run))
		must.Eq(t, d1.ID, bCanary.DeploymentID, must.Sprintf("run %d: the canary took the deployment of the downgraded placement", run))
	}
}
