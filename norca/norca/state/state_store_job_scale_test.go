// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestStateStore_ScaleJob(t *testing.T) {
	ci.Parallel(t)

	state := testStateStore(t)
	job := mock.Job()
	group := job.TaskGroups[0].Name
	sub := &structs.JobSubmission{Source: "job {}", Format: "hcl2"}
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 10, sub, job))
	must.NoError(t, state.UpdateJobStability(11, job.Namespace, job.ID, 0, true))

	scaled, err := state.ScaleJob(structs.MsgTypeTestSetup, 20, job.Namespace, job.ID, group, 15, 1234)
	must.NoError(t, err)
	must.NotNil(t, scaled)

	stored, err := state.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, 15, stored.TaskGroups[0].Count)
	must.Eq(t, int64(1234), stored.SubmitTime)

	// It is the version it was, the stable one, changed at the index of the
	// scaling; its definition was not.
	must.Eq(t, uint64(0), stored.Version)
	must.True(t, stored.Stable)
	must.Eq(t, uint64(10), stored.CreateIndex)
	must.Eq(t, uint64(10), stored.JobModifyIndex)
	must.Eq(t, uint64(20), stored.ModifyIndex)

	// The tracked versions hold the one version, with the new count, and its
	// submission is still there.
	versions, err := state.JobVersionsByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 1, versions)
	must.Eq(t, 15, versions[0].TaskGroups[0].Count)
	got, err := state.JobSubmission(nil, job.Namespace, job.ID, 0)
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, "job {}", got.Source)

	// The summary of the job follows as it does for any update of the job.
	summary, err := state.JobSummaryByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.MapContainsKey(t, summary.Summary, group)
}

// The count is set on the job that is stored when the request is applied: a
// version registered since the request was made keeps its definition.
func TestStateStore_ScaleJob_NewerVersion(t *testing.T) {
	ci.Parallel(t)

	state := testStateStore(t)
	job := mock.Job()
	group := job.TaskGroups[0].Name
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 10, nil, job))

	newer := job.Copy()
	newer.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 15, nil, newer))

	_, err := state.ScaleJob(structs.MsgTypeTestSetup, 20, job.Namespace, job.ID, group, 3, 0)
	must.NoError(t, err)

	stored, err := state.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, uint64(1), stored.Version)
	must.Eq(t, 3, stored.TaskGroups[0].Count)
	must.Eq(t, "/bin/other", stored.TaskGroups[0].Tasks[0].Config["command"])
}

func TestStateStore_ScaleJob_Gone(t *testing.T) {
	ci.Parallel(t)

	state := testStateStore(t)
	job := mock.Job()
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 10, nil, job))

	tests := []struct {
		name       string
		job, group string
	}{
		{name: "no such job", job: "other", group: job.TaskGroups[0].Name},
		{name: "no such group", job: job.ID, group: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaled, err := state.ScaleJob(structs.MsgTypeTestSetup, 20, job.Namespace, tt.job, tt.group, 3, 0)
			must.NoError(t, err)
			must.Nil(t, scaled)

			stored, err := state.JobByID(nil, job.Namespace, job.ID)
			must.NoError(t, err)
			must.Eq(t, uint64(10), stored.ModifyIndex)
		})
	}
}

// What scaling adds to a group is recorded on the deployment of the job's
// version while that is the deployment of the group; nothing else of the
// deployment changes.
func TestStateStore_ScaleJob_Deployment(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		counts   []int // the group has 10; scaled to these in turn
		change   func(d *structs.Deployment)
		expected int
	}{
		{name: "a scale up during a running deployment", counts: []int{13}, expected: 3},
		{name: "two scale ups add up", counts: []int{12, 15}, expected: 5},
		{name: "a scale down takes away what was added first", counts: []int{13, 11}, expected: 1},
		{name: "and no more than that", counts: []int{13, 8, 9}, expected: 1},
		{name: "a paused deployment", counts: []int{13}, expected: 3,
			change: func(d *structs.Deployment) { d.Status = structs.DeploymentStatusPaused }},
		{name: "a failed deployment is still the deployment of the group", counts: []int{13}, expected: 3,
			change: func(d *structs.Deployment) { d.Status = structs.DeploymentStatusFailed }},
		{name: "a deployment that succeeded is over", counts: []int{13}, expected: 0,
			change: func(d *structs.Deployment) { d.Status = structs.DeploymentStatusSuccessful }},
		{name: "a deployment that was cancelled is over", counts: []int{13}, expected: 0,
			change: func(d *structs.Deployment) { d.Status = structs.DeploymentStatusCancelled }},
		{name: "a deployment of another version is left alone", counts: []int{13}, expected: 0,
			change: func(d *structs.Deployment) { d.JobVersion = 7 }},
		{name: "a deployment without the group is left alone", counts: []int{13}, expected: 0,
			change: func(d *structs.Deployment) {
				d.TaskGroups["other"] = d.TaskGroups["web"]
				delete(d.TaskGroups, "web")
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := testStateStore(t)
			job := mock.Job() // the group web, count 10
			must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 10, nil, job))

			d := structs.NewDeployment(job, 50)
			d.TaskGroups["web"] = &structs.DeploymentState{DesiredTotal: 10, HealthyAllocs: 4, PlacedAllocs: 6}
			if tt.change != nil {
				tt.change(d)
			}
			status := d.Status
			must.NoError(t, state.UpsertDeployment(11, d))

			for i, count := range tt.counts {
				_, err := state.ScaleJob(structs.MsgTypeTestSetup, uint64(20+i), job.Namespace, job.ID, "web", count, 0)
				must.NoError(t, err)
			}

			stored, err := state.DeploymentByID(nil, d.ID)
			must.NoError(t, err)
			must.Eq(t, status, stored.Status)
			var dstate *structs.DeploymentState
			for _, s := range stored.TaskGroups {
				dstate = s
			}
			must.Eq(t, &structs.DeploymentState{
				DesiredTotal: 10, HealthyAllocs: 4, PlacedAllocs: 6, ScaleAdded: tt.expected,
			}, dstate)
			if tt.expected == 0 {
				must.Eq(t, uint64(11), stored.ModifyIndex)
			}
		})
	}
}
