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
)

// Two jobs that separate each other by zone, evaluated in parallel on the same
// snapshot: the plan applier checks the rule again on its state, so the second
// plan is refused instead of putting both jobs into one zone (review finding
// SCH-6).
func TestPlanApply_SeparateRuleCheckedAgain(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.EnabledSchedulers = []string{structs.JobTypeService}
	})
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)

	node := mock.Node()
	node.Meta["zone"] = "z0"
	must.NoError(t, node.ComputeClass())
	testRegisterNode(t, s1, node)

	sep := func(app string) []*structs.AllocAffinity {
		return []*structs.AllocAffinity{{Selector: map[string]string{"app": app}, Scope: "${meta.zone}", Required: true}}
	}
	jobX := affJob("x")
	jobX.TaskGroups[0].Separate = sep("y")
	jobY := affJob("y")
	jobY.TaskGroups[0].Separate = sep("x")
	jobZ := affJob("z") // control: separate from x, evaluated on a fresh snapshot
	jobZ.TaskGroups[0].Separate = sep("x")

	st := s1.fsm.State()
	codec := rpcClient(t, s1)
	evals := map[string]*structs.Evaluation{}
	tokens := map[string]string{}
	// Job.Register writes the job and its evaluation through Raft and
	// enqueues the evaluation; no scheduler worker runs (NumSchedulers=0).
	register := func(job *structs.Job) {
		req := &structs.JobRegisterRequest{Job: job, WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace}}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
		out, token, err := s1.evalBroker.Dequeue([]string{job.Type}, 5*time.Second)
		must.NoError(t, err)
		must.NotNil(t, out)
		must.Eq(t, job.ID, out.JobID)
		evals[job.ID], tokens[job.ID] = out, token
	}
	register(jobX)
	register(jobY)
	tokX, tokY := tokens[jobX.ID], tokens[jobY.ID]

	// Both workers took their snapshot before either plan was applied.
	snap, err := st.Snapshot()
	must.NoError(t, err)
	poolArgs := getSchedulerWorkerPoolArgsFromConfigLocked(s1.config).Copy()
	w1 := newWorker(s1.shutdownCtx, s1, poolArgs)
	w2 := newWorker(s1.shutdownCtx, s1, poolArgs)
	must.NoError(t, w1.invokeScheduler(snap, evals[jobX.ID], tokX))
	must.NoError(t, w2.invokeScheduler(snap, evals[jobY.ID], tokY))

	// The plan of the second job was made on a snapshot without the first
	// one's allocation; the plan applier refuses it, and the scheduler,
	// planning again on the newer state, places nothing in that zone.
	ax, ay := affLiveAllocs(t, s1, jobX), affLiveAllocs(t, s1, jobY)
	must.Eq(t, 1, len(ax))
	must.Eq(t, 0, len(ay), must.Sprint("the second job was placed in the zone of the first"))
}
