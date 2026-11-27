// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// testTracedCluster starts three servers with tracing and returns them with
// the leader and a follower.
func testTracedCluster(t *testing.T, tracing *telemetry.Tracing, cb func(*Config)) (leader, follower *Server, servers []*Server) {
	t.Helper()
	servers = make([]*Server, 3)
	for i := range servers {
		s, cleanup := TestServer(t, func(c *Config) {
			c.BootstrapExpect = 3
			c.Tracing = tracing
			if cb != nil {
				cb(c)
			}
		})
		t.Cleanup(cleanup)
		servers[i] = s
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	for _, s := range servers {
		if s.IsLeader() {
			leader = s
		} else if follower == nil {
			follower = s
		}
	}
	must.NotNil(t, leader)
	must.NotNil(t, follower)
	return leader, follower, servers
}

// spanWithAttr waits for the span named name whose attribute key is value
// and returns it.
func spanWithAttr(t *testing.T, exp *tracetest.InMemoryExporter, name string, key, value string) tracetest.SpanStub {
	t.Helper()
	var found tracetest.SpanStub
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			for _, s := range exp.GetSpans() {
				if s.Name == name && attrKey(s, key) == value {
					found = s
					return true
				}
			}
			return false
		}),
		wait.Timeout(20*time.Second),
		wait.Gap(20*time.Millisecond),
	), must.Sprintf("span %s with %s=%s", name, key, value))
	return found
}

func attrKey(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// isAncestor reports whether span a is an ancestor of span b in spans.
func isAncestor(spans map[trace.SpanID]tracetest.SpanStub, a, b tracetest.SpanStub) bool {
	for cur := b; cur.Parent.IsValid(); {
		parent, ok := spans[cur.Parent.SpanID()]
		if !ok {
			return false
		}
		if parent.SpanContext.SpanID() == a.SpanContext.SpanID() {
			return true
		}
		cur = parent
	}
	return false
}

// The trace of a job registered through a follower continues into the
// scheduling: the leader's FSM apply of the registration → eval.process on
// a worker → scheduler.service → scheduler.compute → scheduler.select, and
// plan.submit → Plan.Submit → plan.evaluate and plan.apply on the leader →
// the FSM apply of the plan results; the worker's ack is a child of
// eval.process.
func TestTracing_JobRunScheduled(t *testing.T) {
	ci.Parallel(t)

	tracing, exp := newTestTracing(t)
	leader, follower, _ := testTracedCluster(t, tracing, nil)

	node := mock.Node()
	nodeReq := &structs.NodeRegisterRequest{Node: node, WriteRequest: structs.WriteRequest{Region: "global"}}
	var nodeResp structs.NodeUpdateResponse
	must.NoError(t, leader.RPC("Node.Register", nodeReq, &nodeResp))

	job := mock.Job()
	job.TaskGroups[0].Count = 1
	_, root := tracing.Tracer().Start(context.Background(), "PUT /v1/jobs", trace.WithSpanKind(trace.SpanKindServer))
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace},
	}
	req.SetTraceParent(telemetry.FormatTraceParent(root.SpanContext()))
	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, follower), "Job.Register", req, &resp))
	root.End()
	must.NotEq(t, "", resp.EvalID)

	process := spanWithAttr(t, exp, telemetry.SpanEvalProcess, string(telemetry.AttrEvalID), resp.EvalID)
	rootSC := root.SpanContext()
	must.Eq(t, rootSC.TraceID(), process.SpanContext.TraceID(), must.Sprint("eval.process joins the job's trace"))
	must.Eq(t, job.ID, attr(process, telemetry.AttrJobID))
	must.Eq(t, structs.DefaultNamespace, attr(process, telemetry.AttrNamespace))
	must.Eq(t, structs.EvalTriggerJobRegister, attr(process, telemetry.AttrTriggeredBy))
	must.Eq(t, structs.JobTypeService, attr(process, telemetry.AttrSchedulerType))
	must.Eq(t, structs.EvalStatusComplete, attr(process, telemetry.AttrEvalStatus))
	must.NotEq(t, codes.Error, process.Status.Code)

	spans := traceSpans(exp, rootSC.TraceID())
	parent, ok := spans[process.Parent.SpanID()]
	must.True(t, ok, must.Sprint("the parent of eval.process is in the trace"))
	must.Eq(t, "fsm.apply JobRegisterRequestType", parent.Name)

	sched := childNamed(t, spans, process.SpanContext.SpanID(), "scheduler.service")
	compute := childNamed(t, spans, sched.SpanContext.SpanID(), telemetry.SpanSchedulerCompute)
	sel := childNamed(t, spans, compute.SpanContext.SpanID(), telemetry.SpanSchedulerSelect)
	must.Eq(t, "web", attr(sel, telemetry.AttrTaskGroup))
	must.Eq(t, "1", attr(sel, telemetry.AttrPlaced))
	must.Eq(t, "0", attr(sel, telemetry.AttrFailed))

	submit := childNamed(t, spans, sched.SpanContext.SpanID(), telemetry.SpanPlanSubmit)
	must.Eq(t, trace.SpanKindClient, submit.SpanKind)
	submitRPC := childNamed(t, spans, submit.SpanContext.SpanID(), "Plan.Submit")

	var evaluate, apply tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case telemetry.SpanPlanEvaluate:
			evaluate = s
		case telemetry.SpanPlanApply:
			apply = s
		}
	}
	must.True(t, isAncestor(spans, submitRPC, evaluate), must.Sprint("plan.evaluate below Plan.Submit"))
	must.True(t, isAncestor(spans, submitRPC, apply), must.Sprint("plan.apply below Plan.Submit"))
	must.Eq(t, resp.EvalID, attr(apply, telemetry.AttrEvalID))
	must.NotEq(t, "", attr(apply, "norca.plan.alloc_index"))
	childNamed(t, spans, apply.SpanContext.SpanID(), "fsm.apply ApplyPlanResultsRequestType")

	// the worker's ack, at the end of eval.process
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			return slices.ContainsFunc(exp.GetSpans(), func(s tracetest.SpanStub) bool {
				return s.Name == "Eval.Ack" && s.Parent.SpanID() == process.SpanContext.SpanID()
			})
		}),
		wait.Timeout(5*time.Second),
	))
}

// After a leader change, the evaluations the new leader enqueues again from
// the state have no trace parent: they are processed in new root traces
// with their norca.eval.id, without errors.
func TestTracing_LeaderChangeRootEval(t *testing.T) {
	ci.Parallel(t)

	tracing, exp := newTestTracing(t)
	// the workers process the core evaluations only until the leader
	// changes, so the job's evaluation waits in the broker
	coreOnly := SchedulerWorkerPoolArgs{NumSchedulers: 1, EnabledSchedulers: []string{structs.JobTypeCore}}
	leader, _, servers := testTracedCluster(t, tracing, func(c *Config) {
		c.NumSchedulers = coreOnly.NumSchedulers
		c.EnabledSchedulers = coreOnly.EnabledSchedulers
	})

	job := mock.Job()
	job.TaskGroups[0].Count = 1
	_, root := tracing.Tracer().Start(context.Background(), "PUT /v1/jobs", trace.WithSpanKind(trace.SpanKindServer))
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace},
	}
	req.SetTraceParent(telemetry.FormatTraceParent(root.SpanContext()))
	var resp structs.JobRegisterResponse
	must.NoError(t, leader.RPC("Job.Register", req, &resp))
	root.End()
	must.Eq(t, 1, tracedEvals(leader.evalBroker), must.Sprint("the leader keeps the parent"))

	must.NoError(t, leader.raft.LeadershipTransfer().Error())
	var newLeader *Server
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			for _, s := range servers {
				if s != leader && s.IsLeader() && s.evalBroker.Enabled() {
					newLeader = s
					return true
				}
			}
			return false
		}),
		wait.Timeout(20*time.Second),
	))
	must.Eq(t, 0, tracedEvals(leader.evalBroker), must.Sprint("the old leader flushed its broker"))
	must.Eq(t, 0, tracedEvals(newLeader.evalBroker), must.Sprint("nothing replicated"))

	all := SchedulerWorkerPoolArgs{NumSchedulers: 1, EnabledSchedulers: []string{
		structs.JobTypeService, structs.JobTypeBatch, structs.JobTypeSystem, structs.JobTypeSysBatch, structs.JobTypeCore,
	}}
	for _, s := range servers {
		s.SetSchedulerWorkerConfig(all)
	}

	process := spanWithAttr(t, exp, telemetry.SpanEvalProcess, string(telemetry.AttrEvalID), resp.EvalID)
	must.False(t, process.Parent.IsValid(), must.Sprint("a new root"))
	must.NotEq(t, root.SpanContext().TraceID(), process.SpanContext.TraceID())
	must.NotEq(t, codes.Error, process.Status.Code)
	must.Eq(t, job.ID, attr(process, telemetry.AttrJobID))
}
