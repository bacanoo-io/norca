// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestNamespace_Validate_Oss(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name        string
		namespace   *Namespace
		expectedErr string
	}{
		{
			name: "node pool config not allowed",
			namespace: &Namespace{
				Name: "test",
				NodePoolConfiguration: &NamespaceNodePoolConfiguration{
					Default: "dev",
				},
			},
			expectedErr: "not supported",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.namespace.Validate()
			if tc.expectedErr != "" {
				must.ErrorContains(t, err, tc.expectedErr)
			} else {
				must.NoError(t, err)
			}
		})
	}
}

// TestJob_Validate_Multiregion_Oss asserts that a job with a multiregion block,
// even an empty one, is rejected until multiregion deployments are supported.
func TestJob_Validate_Multiregion_Oss(t *testing.T) {
	ci.Parallel(t)

	for _, m := range []*Multiregion{
		{},
		{Strategy: &MultiregionStrategy{MaxParallel: 1}},
		{Regions: []*MultiregionRegion{{Name: "west"}, {Name: "east"}}},
	} {
		job := testJob()
		job.Multiregion = m
		must.ErrorContains(t, job.Validate(), ErrMultiregionNotSupported.Error())
	}

	job := testJob()
	job.Multiregion = nil
	must.NoError(t, job.Validate())
}

// TestJob_Validate_ConsulNamespace_Oss asserts that a Consul namespace set on
// the job or a task group is rejected: it would be silently ignored.
func TestJob_Validate_ConsulNamespace_Oss(t *testing.T) {
	ci.Parallel(t)

	job := testJob()
	job.ConsulNamespace = "apple"
	must.ErrorContains(t, job.Validate(), ErrConsulNamespaceNotSupported.Error())

	job = testJob()
	job.TaskGroups[0].Consul = &Consul{Namespace: "banana"}
	must.ErrorContains(t, job.Validate(), ErrConsulNamespaceNotSupported.Error())

	job = testJob()
	job.TaskGroups[0].Consul = &Consul{}
	must.NoError(t, job.Validate())
}
