// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestMultiregion_Validate(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name    string
		m       *Multiregion
		jobDCs  []string
		wantErr []string
	}{
		{
			name: "nil",
		},
		{
			name:    "no regions",
			m:       &Multiregion{Strategy: &MultiregionStrategy{MaxParallel: 1}},
			wantErr: []string{"must have at least one region"},
		},
		{
			name: "valid",
			m: &Multiregion{
				Strategy: &MultiregionStrategy{MaxParallel: 1, OnFailure: MultiregionOnFailureFailAll},
				Regions: []*MultiregionRegion{
					{Name: "aws", Count: 2, Datacenters: []string{"aws-1"}},
					{Name: "gcp"},
				},
			},
			jobDCs: []string{"dc1"},
		},
		{
			name: "invalid strategy and regions",
			m: &Multiregion{
				Strategy: &MultiregionStrategy{MaxParallel: -1, OnFailure: "fail_some"},
				Regions: []*MultiregionRegion{
					{Name: "aws", Count: -1},
					{Name: "aws", Datacenters: []string{""}},
					{},
				},
			},
			wantErr: []string{
				"max_parallel must be non-negative",
				"on_failure must be",
				`region "aws" count must be non-negative`,
				`region "aws" is listed more than once`,
				`region "aws" must have datacenters when the job has none`,
				`region "aws" datacenter must be non-empty string`,
				"region 3 must have a name",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate(JobTypeService, tc.jobDCs)
			if len(tc.wantErr) == 0 {
				must.NoError(t, err)
				return
			}
			for _, want := range tc.wantErr {
				must.ErrorContains(t, err, want)
			}
		})
	}
}

func TestJob_InterpolateMultiregion(t *testing.T) {
	ci.Parallel(t)

	job := testJob()
	job.Region = "global"
	job.Datacenters = []string{"dc1"}
	job.NodePool = "default"
	job.Meta = map[string]string{"first": "job", "second": "job"}
	job.TaskGroups[0].Count = 0
	job.Multiregion = &Multiregion{
		Regions: []*MultiregionRegion{
			{
				Name:        "aws",
				Count:       3,
				Datacenters: []string{"aws-1", "aws-2"},
				NodePool:    "gpu",
				Meta:        map[string]string{"first": "aws"},
			},
			{Name: "gcp", Count: 5},
		},
	}

	aws, err := job.InterpolateMultiregion("aws")
	must.NoError(t, err)
	must.Eq(t, "aws", aws.Region)
	must.Eq(t, []string{"aws-1", "aws-2"}, aws.Datacenters)
	must.Eq(t, "gpu", aws.NodePool)
	must.Eq(t, map[string]string{"first": "aws", "second": "job"}, aws.Meta)
	must.Eq(t, 3, aws.TaskGroups[0].Count)
	must.Eq(t, job.Multiregion.Copy(), aws.Multiregion)

	// Interpolating again for the same region changes nothing.
	again, err := aws.InterpolateMultiregion("aws")
	must.NoError(t, err)
	must.False(t, aws.SpecChanged(again))

	// A region without overrides keeps the job fields.
	gcp, err := job.InterpolateMultiregion("gcp")
	must.NoError(t, err)
	must.Eq(t, "gcp", gcp.Region)
	must.Eq(t, []string{"dc1"}, gcp.Datacenters)
	must.Eq(t, "default", gcp.NodePool)
	must.Eq(t, map[string]string{"first": "job", "second": "job"}, gcp.Meta)
	must.Eq(t, 5, gcp.TaskGroups[0].Count)

	// A task group with its own count keeps it.
	job.TaskGroups[0].Count = 1
	gcp, err = job.InterpolateMultiregion("gcp")
	must.NoError(t, err)
	must.Eq(t, 1, gcp.TaskGroups[0].Count)

	// The job itself is not modified.
	must.Eq(t, "global", job.Region)
	must.Eq(t, map[string]string{"first": "job", "second": "job"}, job.Meta)

	_, err = job.InterpolateMultiregion("hetzner")
	must.ErrorContains(t, err, `region "hetzner" is not one of the regions`)
}

func TestJob_UsesMultiregionDeployments(t *testing.T) {
	ci.Parallel(t)

	job := testJob()
	job.Periodic = nil
	must.False(t, job.UsesMultiregionDeployments())

	job.Multiregion = &Multiregion{Regions: []*MultiregionRegion{{Name: "aws"}}}
	job.TaskGroups[0].Update = &UpdateStrategy{MaxParallel: 1}
	must.True(t, job.UsesMultiregionDeployments())

	d := NewMultiregionDeployment(job, 50)
	must.True(t, d.IsMultiregion)
	must.Eq(t, DeploymentStatusInitializing, d.Status)

	job.TaskGroups[0].Update = nil
	job.Update = UpdateStrategy{}
	must.False(t, job.UsesMultiregionDeployments())

	job.TaskGroups[0].Update = &UpdateStrategy{MaxParallel: 1}
	job.Type = JobTypeBatch
	must.False(t, job.UsesMultiregionDeployments())
}
