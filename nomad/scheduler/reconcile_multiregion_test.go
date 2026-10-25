// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// TestReconciler_MultiregionDeploymentComplete asserts that a complete
// multiregion deployment is blocked, not successful, and only when it is
// running; and that a deployment of a multiregion job that is not part of a
// multiregion rollout completes as successful.
func TestReconciler_MultiregionDeploymentComplete(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name          string
		isMultiregion bool
		status        string
		want          []*structs.DeploymentStatusUpdate
	}{
		{
			name:          "running multiregion deployment is blocked",
			isMultiregion: true,
			status:        structs.DeploymentStatusRunning,
			want: []*structs.DeploymentStatusUpdate{{
				Status:            structs.DeploymentStatusBlocked,
				StatusDescription: structs.DeploymentStatusDescriptionBlocked,
			}},
		},
		{
			name:          "pending multiregion deployment waits",
			isMultiregion: true,
			status:        structs.DeploymentStatusPending,
		},
		{
			name:          "blocked multiregion deployment stays blocked",
			isMultiregion: true,
			status:        structs.DeploymentStatusBlocked,
		},
		{
			name:          "deployment outside of a multiregion rollout is successful",
			isMultiregion: false,
			status:        structs.DeploymentStatusRunning,
			want: []*structs.DeploymentStatusUpdate{{
				Status:            structs.DeploymentStatusSuccessful,
				StatusDescription: structs.DeploymentStatusDescriptionSuccessful,
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := mock.MultiregionJob()
			job.TaskGroups[0].Update = noCanaryUpdate

			d := structs.NewDeployment(job, 50)
			d.IsMultiregion = tc.isMultiregion
			d.Status = tc.status
			d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
				Promoted:      true,
				DesiredTotal:  10,
				PlacedAllocs:  10,
				HealthyAllocs: 10,
			}

			var allocs []*structs.Allocation
			for i := 0; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = uuid.Generate()
				alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				alloc.TaskGroup = job.TaskGroups[0].Name
				alloc.DeploymentID = d.ID
				alloc.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
				allocs = append(allocs, alloc)
			}

			reconciler := NewAllocReconciler(testlog.HCLogger(t), allocUpdateFnIgnore, false, job.ID,
				job, d, allocs, nil, "", 50, true)
			r := reconciler.Compute()

			for _, u := range tc.want {
				u.DeploymentID = d.ID
			}
			must.Eq(t, tc.want, r.deploymentUpdates)
		})
	}
}
