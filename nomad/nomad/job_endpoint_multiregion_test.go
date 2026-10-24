// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// TestJobEndpoint_Multiregion_NotSupported asserts that a job with a
// multiregion block is rejected by Job.Register, Job.Plan and Job.Validate
// instead of the block being silently ignored.
func TestJobEndpoint_Multiregion_NotSupported(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	cases := []struct {
		name        string
		multiregion *structs.Multiregion
	}{
		{
			name: "with regions",
			multiregion: &structs.Multiregion{
				Strategy: &structs.MultiregionStrategy{MaxParallel: 1},
				Regions: []*structs.MultiregionRegion{
					{Name: "global", Count: 1, Datacenters: []string{"dc1"}},
					{Name: "west", Count: 1, Datacenters: []string{"dc1"}},
				},
			},
		},
		{
			name: "strategy only",
			multiregion: &structs.Multiregion{
				Strategy: &structs.MultiregionStrategy{MaxParallel: 1},
			},
		},
		{
			name:        "empty block",
			multiregion: &structs.Multiregion{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := mock.Job()
			job.Multiregion = tc.multiregion
			writeReq := structs.WriteRequest{
				Region:    "global",
				Namespace: job.Namespace,
			}

			regReq := &structs.JobRegisterRequest{Job: job, WriteRequest: writeReq}
			var regResp structs.JobRegisterResponse
			err := msgpackrpc.CallWithCodec(codec, "Job.Register", regReq, &regResp)
			must.ErrorContains(t, err, structs.ErrMultiregionNotSupported.Error())

			planReq := &structs.JobPlanRequest{Job: job.Copy(), Diff: true, WriteRequest: writeReq}
			var planResp structs.JobPlanResponse
			err = msgpackrpc.CallWithCodec(codec, "Job.Plan", planReq, &planResp)
			must.ErrorContains(t, err, structs.ErrMultiregionNotSupported.Error())

			valReq := &structs.JobValidateRequest{Job: job.Copy(), WriteRequest: writeReq}
			var valResp structs.JobValidateResponse
			must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Validate", valReq, &valResp))
			must.StrContains(t, valResp.Error, structs.ErrMultiregionNotSupported.Error())

			// Nothing was stored.
			stored, err := s1.State().JobByID(nil, job.Namespace, job.ID)
			must.NoError(t, err)
			must.Nil(t, stored)
		})
	}
}
