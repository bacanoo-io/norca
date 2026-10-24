// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestHTTP_Job_Multiregion asserts that the HTTP agent registers a multiregion
// job in the regions it lists, with its region-specific fields, and rejects a
// multiregion block without regions.
func TestHTTP_Job_Multiregion(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		// A multiregion block needs regions.
		job := MockJob()
		job.Multiregion = &api.Multiregion{}
		buf := encodeReq(api.JobRegisterRequest{Job: job})
		req, err := http.NewRequest(http.MethodPut, "/v1/jobs", buf)
		must.NoError(t, err)
		_, err = s.Server.JobsRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "Multiregion block must have at least one region")

		// A job that lists only this region is registered here with the
		// fields of the region.
		job = MockJob()
		job.Region = nil
		job.Multiregion = &api.Multiregion{
			Strategy: &api.MultiregionStrategy{MaxParallel: pointer.Of(1)},
			Regions: []*api.MultiregionRegion{{
				Name:        "global",
				Count:       pointer.Of(3),
				Datacenters: []string{"dc2"},
				Meta:        map[string]string{"region": "global"},
			}},
		}
		job.TaskGroups[0].Count = pointer.Of(0)
		job.Update = api.DefaultUpdateStrategy()
		buf = encodeReq(api.JobRegisterRequest{Job: job})
		req, err = http.NewRequest(http.MethodPut, "/v1/jobs", buf)
		must.NoError(t, err)
		_, err = s.Server.JobsRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)

		state := s.Agent.server.State()
		stored, err := state.JobByID(nil, structs.DefaultNamespace, *job.ID)
		must.NoError(t, err)
		must.NotNil(t, stored)
		must.Eq(t, "global", stored.Region)
		must.Eq(t, []string{"dc2"}, stored.Datacenters)
		must.Eq(t, 3, stored.TaskGroups[0].Count)
		must.Eq(t, "global", stored.Meta["region"])

		// The registration created a multiregion deployment, which the
		// scheduler moves to pending and the deployment watcher starts.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			d, err := state.LatestDeploymentByJobID(nil, structs.DefaultNamespace, *job.ID)
			if err != nil {
				return err
			}
			if d == nil || !d.IsMultiregion {
				return fmt.Errorf("no multiregion deployment: %#v", d)
			}
			if d.Status != structs.DeploymentStatusRunning {
				return fmt.Errorf("deployment is %s", d.Status)
			}
			return nil
		})))
	})
}
