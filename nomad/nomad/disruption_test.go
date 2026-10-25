// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net/http"
	"sync"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func upsertDisruptionTestJob(t *testing.T, store *state.StateStore, index uint64, count int,
	budget *structs.DisruptionBudget) (*structs.Job, []*structs.Allocation) {
	t.Helper()

	job := mock.Job()
	job.TaskGroups[0].Count = count
	job.TaskGroups[0].Disruption = budget
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))

	allocs := make([]*structs.Allocation, count)
	for i := range allocs {
		a := mock.Alloc()
		a.Job = job
		a.JobID = job.ID
		a.TaskGroup = job.TaskGroups[0].Name
		a.ClientStatus = structs.AllocClientStatusRunning
		a.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(true)}
		allocs[i] = a
	}
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index+1, allocs))
	return job, allocs
}

// TestAllocEndpoint_Evict_Parallel asserts that of parallel eviction
// requests only as many succeed as the budget allows, the rest get 429.
func TestAllocEndpoint_Evict_Parallel(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	job, allocs := upsertDisruptionTestJob(t, s1.fsm.State(), 1000, 6,
		&structs.DisruptionBudget{MinHealthy: "4"})

	type result struct {
		resp structs.AllocEvictResponse
		err  error
	}
	results := make([]result, len(allocs))
	var wg sync.WaitGroup
	for i, alloc := range allocs {
		wg.Add(1)
		go func(i int, allocID string) {
			defer wg.Done()
			codec := rpcClient(t, s1)
			req := &structs.AllocEvictRequest{AllocID: allocID}
			req.Namespace = job.Namespace
			req.Region = "global"
			results[i].err = msgpackrpc.CallWithCodec(codec, "Alloc.Evict", req, &results[i].resp)
		}(i, alloc.ID)
	}
	wg.Wait()

	succeeded := 0
	for _, r := range results {
		if r.err == nil {
			succeeded++
			must.NotEq(t, "", r.resp.EvalID)
			must.Positive(t, r.resp.Index)
			continue
		}
		code, _, ok := structs.CodeFromRPCCodedErr(r.err)
		must.True(t, ok, must.Sprintf("unexpected error: %v", r.err))
		must.Eq(t, http.StatusTooManyRequests, code)
	}
	must.Eq(t, 2, succeeded)

	// Exactly the evicted allocations are marked for migration
	stored, err := s1.fsm.State().AllocsByJob(nil, job.Namespace, job.ID, false)
	must.NoError(t, err)
	migrating := 0
	for _, a := range stored {
		if a.DesiredTransition.ShouldMigrate() {
			migrating++
		}
	}
	must.Eq(t, 2, migrating)

	evals, err := s1.fsm.State().EvalsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 2, evals)
	must.Eq(t, structs.EvalTriggerAllocStop, evals[0].TriggeredBy)

	// Evicting an allocation that is already being migrated is a no-op
	for i, r := range results {
		if r.err != nil {
			continue
		}
		req := &structs.AllocEvictRequest{AllocID: allocs[i].ID}
		req.Namespace = job.Namespace
		req.Region = "global"
		var resp structs.AllocEvictResponse
		must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s1), "Alloc.Evict", req, &resp))
		must.Eq(t, "", resp.EvalID)
		break
	}
}

func TestAllocEndpoint_Evict_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanupS1 := TestACLServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	store := s1.fsm.State()
	job, allocs := upsertDisruptionTestJob(t, store, 1000, 2, &structs.DisruptionBudget{MaxUnavailable: "1"})

	req := &structs.AllocEvictRequest{AllocID: allocs[0].ID}
	req.Namespace = job.Namespace
	req.Region = "global"

	var resp structs.AllocEvictResponse
	err := msgpackrpc.CallWithCodec(codec, "Alloc.Evict", req, &resp)
	must.True(t, structs.IsErrPermissionDenied(err), must.Sprintf("got: %v", err))

	readToken := mock.CreatePolicyAndToken(t, store, 1010, "read",
		mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityReadJob}))
	req.AuthToken = readToken.SecretID
	err = msgpackrpc.CallWithCodec(codec, "Alloc.Evict", req, &resp)
	must.True(t, structs.IsErrPermissionDenied(err), must.Sprintf("got: %v", err))

	lifecycleToken := mock.CreatePolicyAndToken(t, store, 1011, "lifecycle",
		mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityAllocLifecycle}))
	req.AuthToken = lifecycleToken.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Alloc.Evict", req, &resp))
	must.NotEq(t, "", resp.EvalID)

	// Job.Disruption requires read-job
	dreq := &structs.JobDisruptionRequest{JobID: job.ID}
	dreq.Namespace = job.Namespace
	dreq.Region = "global"
	var dresp structs.JobDisruptionResponse
	err = msgpackrpc.CallWithCodec(codec, "Job.Disruption", dreq, &dresp)
	must.True(t, structs.IsErrPermissionDenied(err), must.Sprintf("got: %v", err))

	dreq.AuthToken = readToken.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Disruption", dreq, &dresp))
	must.True(t, dresp.JobFound)
	status := dresp.TaskGroups[job.TaskGroups[0].Name]
	must.NotNil(t, status)
	must.Eq(t, 1, status.Healthy)
	must.Eq(t, 1, status.RequiredHealthy)
	must.Eq(t, 0, status.DisruptionsAllowed)
}

func TestJobEndpoint_Disruption(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	// Registration with a valid budget, which blocks every disruption
	job := mock.Job()
	job.TaskGroups[0].Count = 3
	job.TaskGroups[0].Disruption = &structs.DisruptionBudget{MinHealthy: "3"}
	req := &structs.JobRegisterRequest{Job: job}
	req.Namespace = job.Namespace
	req.Region = "global"
	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
	must.StrContains(t, resp.Warnings, "blocks every voluntary disruption")

	stored, err := s1.fsm.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, &structs.DisruptionBudget{MinHealthy: "3"}, stored.TaskGroups[0].Disruption)

	dreq := &structs.JobDisruptionRequest{JobID: job.ID}
	dreq.Namespace = job.Namespace
	dreq.Region = "global"
	var dresp structs.JobDisruptionResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Disruption", dreq, &dresp))
	must.True(t, dresp.JobFound)
	must.MapLen(t, 1, dresp.TaskGroups)
	must.Eq(t, 3, dresp.TaskGroups["web"].RequiredHealthy)

	dreq.JobID = "missing"
	dresp = structs.JobDisruptionResponse{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Disruption", dreq, &dresp))
	must.False(t, dresp.JobFound)

	// Batch jobs do not allow the block
	batch := mock.BatchJob()
	batch.TaskGroups[0].Disruption = &structs.DisruptionBudget{MinHealthy: "1"}
	req = &structs.JobRegisterRequest{Job: batch}
	req.Namespace = batch.Namespace
	req.Region = "global"
	err = msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
	must.ErrorContains(t, err, "does not allow disruption block")
}
