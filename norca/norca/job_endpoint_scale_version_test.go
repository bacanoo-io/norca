// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

func scaleRequest(job *structs.Job, count int64) *structs.JobScaleRequest {
	return &structs.JobScaleRequest{
		JobID:        job.ID,
		Target:       map[string]string{structs.ScalingTargetGroup: job.TaskGroups[0].Name},
		Count:        pointer.Of(count),
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
}

// Scaling a job is not a new version of it: the count changes on the version
// that is stored.
func TestJobEndpoint_Scale_KeepsTheVersion(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	state := s1.fsm.State()

	job := mock.Job()
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))
	must.NoError(t, state.UpdateJobStability(1001, job.Namespace, job.ID, 0, true))

	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Scale", scaleRequest(job, 12), &resp))
	must.NotEq(t, "", resp.EvalID)

	stored, err := state.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, 12, stored.TaskGroups[0].Count)
	must.Eq(t, uint64(0), stored.Version)
	must.True(t, stored.Stable)
	must.Eq(t, uint64(1000), stored.JobModifyIndex)
	must.Eq(t, resp.JobModifyIndex, stored.ModifyIndex)

	versions, err := state.JobVersionsByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 1, versions)

	// The evaluation of the change is there for the scheduler.
	eval, err := state.EvalByID(nil, resp.EvalID)
	must.NoError(t, err)
	must.Eq(t, structs.EvalTriggerScaling, eval.TriggeredBy)
}

// A scale up that the policy allows during a deployment leaves the deployment
// running, what it was the rollout of: the allocations that are added are
// recorded on it, and are not its work.
func TestJobEndpoint_Scale_KeepsTheDeployment(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)
	state := s1.fsm.State()

	job, _ := mock.JobWithScalingPolicy()
	tg := job.TaskGroups[0]
	tg.Scaling.Min, tg.Scaling.Max = 1, 20
	tg.Scaling.ScaleDuringDeployment = structs.ScaleDuringDeploymentUpOnly
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, nil, job))
	job, err := state.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)

	d := structs.NewDeployment(job, 50)
	d.TaskGroups[tg.Name] = &structs.DeploymentState{DesiredTotal: 10, PlacedAllocs: 6, HealthyAllocs: 4}
	must.NoError(t, state.UpsertDeployment(1001, d))

	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Scale", scaleRequest(job, 13), &resp))

	stored, err := state.DeploymentByID(nil, d.ID)
	must.NoError(t, err)
	must.Eq(t, structs.DeploymentStatusRunning, stored.Status)
	must.Eq(t, job.Version, stored.JobVersion)
	must.Eq(t, 10, stored.TaskGroups[tg.Name].DesiredTotal)
	must.Eq(t, 3, stored.TaskGroups[tg.Name].ScaleAdded)
	must.Eq(t, 4, stored.TaskGroups[tg.Name].HealthyAllocs)

	latest, err := state.LatestDeploymentByJobID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, d.ID, latest.ID)
}
