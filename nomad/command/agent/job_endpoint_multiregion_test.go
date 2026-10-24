// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// TestHTTP_Job_Multiregion_NotSupported asserts that the register, plan and
// validate HTTP endpoints reject a job with a multiregion block with a clear
// error, even when the block lists regions that are not part of the cluster.
func TestHTTP_Job_Multiregion_NotSupported(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		newJob := func() *api.Job {
			job := MockJob()
			job.Multiregion = &api.Multiregion{
				Regions: []*api.MultiregionRegion{
					{Name: "east", Datacenters: []string{"east-1"}},
					{Name: "west", Datacenters: []string{"west-1"}},
				},
			}
			return job
		}
		wantErr := structs.ErrMultiregionNotSupported.Error()

		// Register
		job := newJob()
		buf := encodeReq(api.JobRegisterRequest{
			Job:          job,
			WriteRequest: api.WriteRequest{Region: "global"},
		})
		req, err := http.NewRequest(http.MethodPut, "/v1/jobs", buf)
		must.NoError(t, err)
		_, err = s.Server.JobsRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, wantErr)

		// Plan
		job = newJob()
		buf = encodeReq(api.JobPlanRequest{
			Job:  job,
			Diff: true,
			WriteRequest: api.WriteRequest{
				Region:    "global",
				Namespace: api.DefaultNamespace,
			},
		})
		req, err = http.NewRequest(http.MethodPut, "/v1/job/"+*job.ID+"/plan", buf)
		must.NoError(t, err)
		_, err = s.Server.JobSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, wantErr)

		// Validate
		buf = encodeReq(api.JobValidateRequest{
			Job:          newJob(),
			WriteRequest: api.WriteRequest{Region: "global"},
		})
		req, err = http.NewRequest(http.MethodPut, "/v1/validate/job", buf)
		must.NoError(t, err)
		obj, err := s.Server.ValidateJobRequest(httptest.NewRecorder(), req)
		must.NoError(t, err)
		resp := obj.(structs.JobValidateResponse)
		must.StrContains(t, resp.Error, wantErr)
	})
}
