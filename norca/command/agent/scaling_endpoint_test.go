// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/stretchr/testify/require"
)

func TestHTTP_ScalingPoliciesList(t *testing.T) {
	ci.Parallel(t)
	require := require.New(t)
	httpTest(t, nil, func(s *TestAgent) {
		for i := 0; i < 3; i++ {
			// Create the job
			job, _ := mock.JobWithScalingPolicy()

			args := structs.JobRegisterRequest{
				Job: job,
				WriteRequest: structs.WriteRequest{
					Region:    "global",
					Namespace: structs.DefaultNamespace,
				},
			}
			var resp structs.JobRegisterResponse
			require.NoError(s.Agent.RPC("Job.Register", &args, &resp))
		}

		// Make the HTTP request
		req, err := http.NewRequest(http.MethodGet, "/v1/scaling/policies", nil)
		require.NoError(err)

		respW := httptest.NewRecorder()

		// Make the request
		obj, err := s.Server.ScalingPoliciesRequest(respW, req)
		require.NoError(err)

		// Check for the index
		require.NotEmpty(respW.Header().Get("X-Nomad-Index"), "missing index")
		require.NotEmpty(respW.Header().Get("X-Nomad-KnownLeader"), "missing known leader")
		require.NotEmpty(respW.Header().Get("X-Nomad-LastContact"), "missing last contact")

		// Check the list
		l := obj.([]*structs.ScalingPolicyListStub)
		require.Len(l, 3)
	})
}

func TestHTTP_ScalingPoliciesList_Filter(t *testing.T) {
	require := require.New(t)
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		var job *structs.Job
		for i := 0; i < 3; i++ {
			// Create the job
			job, _ = mock.JobWithScalingPolicy()

			args := structs.JobRegisterRequest{
				Job: job,
				WriteRequest: structs.WriteRequest{
					Region:    "global",
					Namespace: structs.DefaultNamespace,
				},
			}
			var resp structs.JobRegisterResponse
			require.NoError(s.Agent.RPC("Job.Register", &args, &resp))
		}

		// Make the HTTP request
		req, err := http.NewRequest(http.MethodGet, "/v1/scaling/policies?job="+job.ID, nil)
		require.NoError(err)
		respW := httptest.NewRecorder()

		// Make the request
		obj, err := s.Server.ScalingPoliciesRequest(respW, req)
		require.NoError(err)

		// Check the list
		l := obj.([]*structs.ScalingPolicyListStub)
		require.Len(l, 1)

		// Request again, with policy type filter
		req, err = http.NewRequest(http.MethodGet, "/v1/scaling/policies?type=cluster", nil)
		require.NoError(err)
		respW = httptest.NewRecorder()

		// Make the request
		obj, err = s.Server.ScalingPoliciesRequest(respW, req)
		require.NoError(err)

		// Check the list
		l = obj.([]*structs.ScalingPolicyListStub)
		require.Len(l, 0)
	})
}

func TestHTTP_ScalingPolicyGet(t *testing.T) {
	ci.Parallel(t)
	require := require.New(t)
	httpTest(t, nil, func(s *TestAgent) {
		// Create the job
		job, p := mock.JobWithScalingPolicy()
		args := structs.JobRegisterRequest{
			Job: job,
			WriteRequest: structs.WriteRequest{
				Region:    "global",
				Namespace: structs.DefaultNamespace,
			},
		}
		var resp structs.JobRegisterResponse
		err := s.Agent.RPC("Job.Register", &args, &resp)
		require.NoError(err)

		// Make the HTTP request
		req, err := http.NewRequest(http.MethodGet, "/v1/scaling/policy/"+p.ID, nil)
		require.NoError(err)
		respW := httptest.NewRecorder()

		// Make the request
		obj, err := s.Server.ScalingPolicySpecificRequest(respW, req)
		require.NoError(err)

		// Check for the index
		require.NotEmpty(respW.Header().Get("X-Nomad-Index"), "missing index")
		require.NotEmpty(respW.Header().Get("X-Nomad-KnownLeader"), "missing known leader")
		require.NotEmpty(respW.Header().Get("X-Nomad-LastContact"), "missing last contact")

		// Check the policy
		require.Equal(p.ID, obj.(*structs.ScalingPolicy).ID)
	})
}

// The last evaluation of a policy is read at /v1/scaling/policy/<id>/status;
// a policy the built-in autoscaler has not evaluated has none.
func TestHTTP_ScalingPolicyStatus(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		job, _ := mock.JobWithScalingPolicy()
		args := structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace},
		}
		var resp structs.JobRegisterResponse
		require.NoError(t, s.Agent.RPC("Job.Register", &args, &resp))

		stored, err := s.Agent.Server().State().JobByID(nil, job.Namespace, job.ID)
		require.NoError(t, err)
		policyID := stored.TaskGroups[0].Scaling.ID

		req, err := http.NewRequest(http.MethodGet, "/v1/scaling/policy/"+policyID+"/status", nil)
		require.NoError(t, err)
		respW := httptest.NewRecorder()
		obj, err := s.Server.ScalingPolicySpecificRequest(respW, req)
		require.NoError(t, err)
		require.Nil(t, obj.(*structs.ScalingPolicyStatus))
		require.NotEmpty(t, respW.Header().Get("X-Nomad-KnownLeader"), "missing known leader")

		req, err = http.NewRequest(http.MethodGet, "/v1/scaling/policy/no-such-policy/status", nil)
		require.NoError(t, err)
		_, err = s.Server.ScalingPolicySpecificRequest(httptest.NewRecorder(), req)
		require.ErrorContains(t, err, "policy not found")
	})
}
