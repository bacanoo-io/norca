// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestDrainer_JobPurgedBeforeDrain drains a node whose last allocation belongs
// to a job purged just before the drain: the server already stopped the
// allocation, the client still runs it. The drain is complete as soon as it
// stopped on the client, not at the deadline. (The drainer watched the jobs of
// the allocations that were not stopped on the server only, while it waited
// for those still running on the client: nothing checked the node again.)
func TestDrainer_JobPurgedBeforeDrain(t *testing.T) {
	ci.Parallel(t)

	srv, cleanupSrv := TestServer(t, nil)
	defer cleanupSrv()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	store := srv.State()

	n1 := mock.Node()
	var nodeResp structs.NodeUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.Register", &structs.NodeRegisterRequest{
		Node: n1, WriteRequest: structs.WriteRequest{Region: "global"},
	}, &nodeResp))

	job := mock.Job()
	job.TaskGroups[0].Count = 1
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}, &regResp))
	waitForPlacedAllocs(t, store, n1.ID, 1)

	// The allocation runs on the client.
	allocs, err := store.AllocsByNode(nil, n1.ID)
	must.NoError(t, err)
	running := allocs[0].Copy()
	running.ClientStatus = structs.AllocClientStatusRunning
	var genResp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.UpdateAlloc", &structs.AllocUpdateRequest{
		Alloc: []*structs.Allocation{running}, WriteRequest: structs.WriteRequest{Region: "global"},
	}, &genResp))

	// Purge the job: the server stops the allocation, the client has not
	// reported it yet.
	var deregResp structs.JobDeregisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Deregister", &structs.JobDeregisterRequest{
		JobID:        job.ID,
		Purge:        true,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}, &deregResp))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			j, err := store.JobByID(nil, job.Namespace, job.ID)
			must.NoError(t, err)
			a, err := store.AllocByID(nil, running.ID)
			must.NoError(t, err)
			return j == nil && a != nil && a.DesiredStatus == structs.AllocDesiredStatusStop &&
				a.ClientStatus == structs.AllocClientStatusRunning
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(50*time.Millisecond),
	))

	// Now the drain, with a deadline far beyond the test.
	var drainResp structs.NodeDrainUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.UpdateDrain", &structs.NodeUpdateDrainRequest{
		NodeID: n1.ID,
		DrainStrategy: &structs.DrainStrategy{
			DrainSpec: structs.DrainSpec{Deadline: time.Hour},
		},
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &drainResp))
	time.Sleep(500 * time.Millisecond) // let the drainer take the node
	node, err := store.NodeByID(nil, n1.ID)
	must.NoError(t, err)
	must.NotNil(t, node.DrainStrategy, must.Sprint("the drain ended while the allocation ran on the client"))

	// The client stops the allocation: the drain is complete.
	complete := running.Copy()
	complete.ClientStatus = structs.AllocClientStatusComplete
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.UpdateAlloc", &structs.AllocUpdateRequest{
		Alloc: []*structs.Allocation{complete}, WriteRequest: structs.WriteRequest{Region: "global"},
	}, &genResp))
	waitForNodeDrainComplete(t, store, n1.ID, nil, 3, "")
}
