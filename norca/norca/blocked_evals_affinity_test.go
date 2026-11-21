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

func TestColocateTargets(t *testing.T) {
	ci.Parallel(t)

	eval := &structs.Evaluation{Namespace: "web", FailedTGAllocs: map[string]*structs.AllocMetric{
		"g": {ConstraintFiltered: map[string]int{
			"colocate {app=db} by ${node.unique.id}":    1,
			"colocate {data/app=cache} by ${meta.zone}": 1,
			"separate {app=web} by ${node.unique.id}":   1,
			"missing drivers":                           1,
		}},
	}}
	targets := colocateTargets(eval)
	must.SliceContainsAll(t, []string{"web", "data"}, targets)
	must.Len(t, 2, targets)
	must.SliceEmpty(t, colocateTargets(&structs.Evaluation{}))
}

// An evaluation blocked by a required colocate rule is unblocked by a plan that
// places allocations in the namespace of the rule, not by one in another
// namespace (review finding SCH-4).
func TestBlockedEvals_UnblockAllocAffinity(t *testing.T) {
	ci.Parallel(t)

	blocked, broker := testBlockedEvals(t)
	e := mock.BlockedEval()
	e.Status = structs.EvalStatusBlocked
	e.ClassEligibility = map[string]bool{"v1:123": true}
	e.FailedTGAllocs = map[string]*structs.AllocMetric{"web": {
		ConstraintFiltered: map[string]int{"colocate {app=db} by ${node.unique.id}": 1},
	}}
	blocked.Block(e)
	must.Eq(t, 1, blocked.Stats().TotalBlocked)

	blocked.UnblockAllocAffinity(map[string]struct{}{"other": {}}, 900)
	must.Eq(t, 1, blocked.Stats().TotalBlocked)

	blocked.UnblockAllocAffinity(map[string]struct{}{e.Namespace: {}}, 1000)
	requireBlockedEvalsEnqueued(t, blocked, broker, 1)

	// An evaluation that was in the scheduler at an earlier index than
	// the placement is put back at once when it blocks.
	late := mock.BlockedEval()
	late.Status = structs.EvalStatusBlocked
	late.SnapshotIndex = 999
	late.FailedTGAllocs = e.FailedTGAllocs
	blocked.Block(late)
	must.Eq(t, 0, blocked.Stats().TotalBlocked)
}

func affJob(app string) *structs.Job {
	job := mock.Job()
	job.Meta = map[string]string{"app": app}
	tg := job.TaskGroups[0]
	tg.Count = 1
	tg.Networks = nil
	tg.Update = nil
	for _, task := range tg.Tasks {
		task.Services = nil
	}
	return job
}

func affLiveAllocs(t *testing.T, s *Server, job *structs.Job) []*structs.Allocation {
	allocs, err := s.fsm.State().AllocsByJob(nil, job.Namespace, job.ID, false)
	must.NoError(t, err)
	var out []*structs.Allocation
	for _, a := range allocs {
		if !a.TerminalStatus() {
			out = append(out, a)
		}
	}
	return out
}

// A job with a required colocate rule, registered before the job it is to
// run next to, is placed once that job is: the plan that places it unblocks
// the evaluation (review finding SCH-4; before, it waited for an unrelated
// change of a node).
func TestBlockedEvals_ColocateWokenByPlacement(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, nil)
	defer cleanup()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	node := mock.Node()
	testRegisterNode(t, s1, node)

	register := func(job *structs.Job) {
		req := &structs.JobRegisterRequest{Job: job, WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace}}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
	}

	jobB := affJob("b")
	jobB.TaskGroups[0].Colocate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "a"}, Scope: "${node.unique.id}", Required: true,
	}}
	register(jobB)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return s1.blockedEvals.Stats().TotalBlocked == 1 }),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	jobA := affJob("a")
	register(jobA)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return len(affLiveAllocs(t, s1, jobA)) == 1 }),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	allocA := affLiveAllocs(t, s1, jobA)[0]

	// A's allocation starts running on the client.
	up := allocA.Copy()
	up.ClientStatus = structs.AllocClientStatusRunning
	var uresp structs.GenericResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.UpdateAlloc",
		&structs.AllocUpdateRequest{Alloc: []*structs.Allocation{up}, WriteRequest: structs.WriteRequest{Region: "global"}}, &uresp))

	// B is placed next to A without anything else happening.
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return len(affLiveAllocs(t, s1, jobB)) == 1 }),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)),
		must.Sprint("B was not placed after A was"))
	must.Eq(t, allocA.NodeID, affLiveAllocs(t, s1, jobB)[0].NodeID)
}
