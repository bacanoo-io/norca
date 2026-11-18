// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/rpc"
	"reflect"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// These tests record which allocations run under the ACL policies bound to a
// job (review findings JOG-2 and JOG-8, checked 2026-11-18): a policy bound to
// a job goes to every allocation whose job, or whose job's parent, has the
// bound job ID (resolvePoliciesForClaims). The children of a parameterized or
// a periodic job have that parent. JOG-8 is decided as it is (the children
// get the parent's policies); JOG-2 refuses a change of a job with bound
// policies by a token without them. Assertions marked CURRENT BEHAVIOUR
// record what the code does for findings still open (MUK-24), not a
// decision; change them with the decision.

// jobBoundFixture is a server with ACLs and helpers to bind policies to a
// job, call its RPCs, place an allocation and read its bound policies.
type jobBoundFixture struct {
	t     *testing.T
	s     *Server
	root  *structs.ACLToken
	codec rpc.ClientCodec
}

func newJobBoundFixture(t *testing.T) (*jobBoundFixture, func()) {
	t.Helper()
	s, root, cleanup := TestACLServer(t, nil)
	testutil.WaitForLeader(t, s.RPC)
	return &jobBoundFixture{t: t, s: s, root: root, codec: rpcClient(t, s)}, cleanup
}

// with returns the fixture for a subtest: its helpers fail that subtest.
func (f *jobBoundFixture) with(t *testing.T) *jobBoundFixture {
	g := *f
	g.t = t
	return &g
}

func (f *jobBoundFixture) nextIndex() uint64 {
	f.t.Helper()
	index, err := f.s.fsm.State().LatestIndex()
	must.NoError(f.t, err)
	return index + 1
}

// bind binds a policy to the job, its group or its task.
func (f *jobBoundFixture) bind(name, rules, ns, jobID, group, task string) {
	f.t.Helper()
	policy := mock.ACLPolicy()
	policy.Name = name
	policy.Rules = rules
	policy.JobACL = &structs.JobACL{Namespace: ns, JobID: jobID, Group: group, Task: task}
	policy.SetHash()
	must.NoError(f.t, f.s.fsm.State().UpsertACLPolicies(structs.MsgTypeTestSetup, f.nextIndex(),
		[]*structs.ACLPolicy{policy}))
}

// token creates a token holding one namespace policy with the capabilities.
func (f *jobBoundFixture) token(name, ns string, caps ...string) *structs.ACLToken {
	f.t.Helper()
	return mock.CreatePolicyAndToken(f.t, f.s.fsm.State(), f.nextIndex(), name,
		mock.NamespacePolicy(ns, "", caps))
}

// job reads a job from the state.
func (f *jobBoundFixture) job(ns, id string) *structs.Job {
	f.t.Helper()
	job, err := f.s.fsm.State().JobByID(nil, ns, id)
	must.NoError(f.t, err)
	must.NotNil(f.t, job, must.Sprintf("job %q", id))
	return job
}

// boundPolicies places an allocation of the stored job in its first group
// and returns the identity claims of its first task and the names of the
// policies those claims resolve to.
func (f *jobBoundFixture) boundPolicies(job *structs.Job) (*structs.IdentityClaims, []string) {
	f.t.Helper()
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.Namespace = job.Namespace
	alloc.TaskGroup = job.TaskGroups[0].Name
	must.NoError(f.t, f.s.fsm.State().UpsertAllocs(structs.MsgTypeTestSetup, f.nextIndex(),
		[]*structs.Allocation{alloc}))
	claims := alloc.ToTaskIdentityClaims(job, job.TaskGroups[0].Tasks[0].Name)
	policies, err := f.s.resolvePoliciesForClaims(claims)
	must.NoError(f.t, err)
	names := make([]string, 0, len(policies))
	for _, p := range policies {
		names = append(names, p.Name)
	}
	return claims, names
}

func (f *jobBoundFixture) register(job *structs.Job, token string) error {
	var resp structs.JobRegisterResponse
	return msgpackrpc.CallWithCodec(f.codec, "Job.Register", &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: token},
	}, &resp)
}

func (f *jobBoundFixture) dispatch(req *structs.JobDispatchRequest) (*structs.JobDispatchResponse, error) {
	var resp structs.JobDispatchResponse
	err := msgpackrpc.CallWithCodec(f.codec, "Job.Dispatch", req, &resp)
	return &resp, err
}

func (f *jobBoundFixture) force(ns, jobID, token string) (*structs.PeriodicForceResponse, error) {
	var resp structs.PeriodicForceResponse
	err := msgpackrpc.CallWithCodec(f.codec, "Periodic.Force", &structs.PeriodicForceRequest{
		JobID:        jobID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token},
	}, &resp)
	return &resp, err
}

// A dispatch-job token, without the policies bound to a parameterized job,
// starts children that run under those policies: the job-wide ones and the
// ones of the child's group and task, as the parent's own allocations would.
// The caller chooses the payload, the values of the meta keys the parent
// declares, and a part of the child's ID; nothing else (review finding JOG-8).
func TestJobBoundPolicies_DispatchChildRunsUnderParentPolicies(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newJobBoundFixture(t)
	defer cleanup()

	ns := structs.DefaultNamespace
	parent := mock.BatchJob()
	parent.ID, parent.Name = "ctrl", "ctrl"
	parent.ParameterizedJob = &structs.ParameterizedJobConfig{
		Payload:      structs.DispatchPayloadOptional,
		MetaOptional: []string{"cmd"},
	}
	parent.TaskGroups[0].Tasks[0].DispatchPayload = &structs.DispatchPayloadConfig{File: "input"}
	must.NoError(t, f.register(parent, f.root.SecretID))
	plain := mock.BatchJob()
	plain.ID, plain.Name = "plain", "plain"
	must.NoError(t, f.register(plain, f.root.SecretID))
	parent = f.job(ns, parent.ID)

	f.bind("ctrl-job", `operator { policy = "write" }`, ns, parent.ID, "", "")
	f.bind("ctrl-group", `node { policy = "read" }`, ns, parent.ID, "web", "")
	f.bind("ctrl-task", `agent { policy = "read" }`, ns, parent.ID, "web", "web")
	f.bind("ctrl-other-group", `quota { policy = "read" }`, ns, parent.ID, "other", "")

	dispatcher := f.token("dispatcher", ns, acl.NamespaceCapabilityDispatchJob)
	dispatcherACL, err := f.s.ResolveACLForToken(dispatcher)
	must.NoError(t, err)
	must.False(t, dispatcherACL.AllowOperatorWrite())

	req := func(jobID string, meta map[string]string) *structs.JobDispatchRequest {
		return &structs.JobDispatchRequest{
			JobID:            jobID,
			Payload:          []byte("caller's input"),
			Meta:             meta,
			IdPrefixTemplate: "mine",
			WriteRequest:     structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: dispatcher.SecretID},
		}
	}

	// What the caller can't do: a meta key the parent doesn't declare, a job
	// that isn't parameterized.
	_, err = f.dispatch(req(parent.ID, map[string]string{"other": "x"}))
	must.ErrorContains(t, err, "unpermitted metadata keys")
	_, err = f.dispatch(req(plain.ID, nil))
	must.ErrorContains(t, err, "not a parameterized job")

	resp, err := f.dispatch(req(parent.ID, map[string]string{"cmd": "caller's value"}))
	must.NoError(t, err)
	child := f.job(ns, resp.DispatchedJobID)

	// What the caller chose, and what it couldn't.
	must.Eq(t, parent.ID, child.ParentID)
	must.StrHasPrefix(t, parent.ID+structs.DispatchLaunchSuffix+"mine-", child.ID)
	must.Eq(t, "caller's value", child.Meta["cmd"])
	must.SliceNotEmpty(t, child.Payload, must.Sprint("the caller's payload, compressed"))
	must.Eq(t, ns, child.Namespace)
	must.Eq(t, parent.Priority, child.Priority)
	must.Eq(t, parent.NodePool, child.NodePool)
	must.True(t, reflect.DeepEqual(parent.TaskGroups, child.TaskGroups),
		must.Sprint("the child's groups and tasks are the parent's"))

	// A child can't be dispatched again: only the parent is parameterized.
	_, err = f.dispatch(req(child.ID, nil))
	must.ErrorContains(t, err, "not a parameterized job")

	// The child's allocation runs under the parent's bound policies, the
	// group- and task-scoped ones included, with the parent as its job.
	claims, names := f.boundPolicies(child)
	must.Eq(t, parent.ID, claims.JobID)
	must.SliceContainsAll(t, names, []string{"ctrl-job", "ctrl-group", "ctrl-task"})
	aclObj, err := f.s.ResolveClaims(claims)
	must.NoError(t, err)
	must.True(t, aclObj.AllowOperatorWrite(),
		must.Sprint("the child holds what the dispatcher's token does not"))
}

// A new version of a child keeps its parent (JOG-7 allows it), so a token
// with submit-job but without the bound policies can change the tasks of a
// dispatched or a periodic child over RPC and run them under the parent's
// policies. JOG-2 looks the bound policies up by the parent's ID, as
// resolvePoliciesForClaims does, so both registrations below are refused
// and the children stay as they were.
func TestJobBoundPolicies_ChildNewVersionKeepsParentPolicies(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newJobBoundFixture(t)
	defer cleanup()

	ns := structs.DefaultNamespace
	team := f.token("team", ns, acl.NamespaceCapabilitySubmitJob, acl.NamespaceCapabilityDispatchJob)

	parameterized := mock.BatchJob()
	parameterized.ID, parameterized.Name = "ctrl", "ctrl"
	parameterized.ParameterizedJob = &structs.ParameterizedJobConfig{}
	must.NoError(t, f.register(parameterized, f.root.SecretID))
	f.bind("ctrl-job", `operator { policy = "write" }`, ns, parameterized.ID, "", "")

	periodic := mock.PeriodicJob()
	periodic.ID, periodic.Name = "cron", "cron"
	must.NoError(t, f.register(periodic, f.root.SecretID))
	f.bind("cron-job", `operator { policy = "write" }`, ns, periodic.ID, "", "")

	dispatched, err := f.dispatch(&structs.JobDispatchRequest{
		JobID:        parameterized.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: team.SecretID},
	})
	must.NoError(t, err)
	forced, err := f.force(ns, periodic.ID, team.SecretID)
	must.NoError(t, err)
	eval, err := f.s.fsm.State().EvalByID(nil, forced.EvalID)
	must.NoError(t, err)

	for _, tc := range []struct {
		name    string
		childID string
		parent  string
		policy  string
	}{
		{name: "dispatched child", childID: dispatched.DispatchedJobID, parent: parameterized.ID, policy: "ctrl-job"},
		{name: "periodic child", childID: eval.JobID, parent: periodic.ID, policy: "cron-job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := f.with(t)
			child := f.job(ns, tc.childID)
			must.Eq(t, tc.parent, child.ParentID)

			update := child.Copy()
			update.Payload = nil // a payload is only dispatched
			update.TaskGroups[0].Tasks[0].Config = map[string]interface{}{"run_for": "1h", "chosen_by": "team"}
			err := f.register(update, team.SecretID)
			must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
			must.ErrorContains(t, err, tc.policy)

			stored := f.job(ns, tc.childID)
			must.Eq(t, tc.parent, stored.ParentID)
			must.MapNotContainsKey(t, stored.TaskGroups[0].Tasks[0].Config, "chosen_by")
			claims, names := f.boundPolicies(stored)
			must.Eq(t, tc.parent, claims.JobID)
			must.SliceContains(t, names, tc.policy)
		})
	}
}

// A dispatch-job token may force a periodic job: its child runs under the
// policies bound to the periodic job. The content of the child is the
// parent's; the caller chooses only the time of the launch.
func TestJobBoundPolicies_PeriodicForceChildRunsUnderParentPolicies(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newJobBoundFixture(t)
	defer cleanup()

	ns := structs.DefaultNamespace
	periodic := mock.PeriodicJob()
	periodic.ID, periodic.Name = "cron", "cron"
	must.NoError(t, f.register(periodic, f.root.SecretID))
	f.bind("cron-job", `operator { policy = "write" }`, ns, periodic.ID, "", "")

	dispatcher := f.token("dispatcher", ns, acl.NamespaceCapabilityDispatchJob)
	forced, err := f.force(ns, periodic.ID, dispatcher.SecretID)
	must.NoError(t, err)
	eval, err := f.s.fsm.State().EvalByID(nil, forced.EvalID)
	must.NoError(t, err)

	child := f.job(ns, eval.JobID)
	must.Eq(t, periodic.ID, child.ParentID)
	must.StrHasPrefix(t, periodic.ID+structs.PeriodicLaunchSuffix, child.ID)
	claims, names := f.boundPolicies(child)
	must.Eq(t, periodic.ID, claims.JobID)
	must.Eq(t, []string{"cron-job"}, names)
}

// A parameterized job that is also periodic: a dispatch makes a periodic
// child, and the launches of that child have the child as their parent.
// CURRENT BEHAVIOUR: the policies are looked up by the direct parent only, so
// these grandchildren run without the policies bound to the parameterized
// job, and their identity names the child, not the bound job.
func TestJobBoundPolicies_PeriodicDispatchGrandchildLosesPolicies(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newJobBoundFixture(t)
	defer cleanup()

	ns := structs.DefaultNamespace
	parent := mock.PeriodicJob()
	parent.ID, parent.Name = "pp", "pp"
	parent.ParameterizedJob = &structs.ParameterizedJobConfig{}
	must.NoError(t, f.register(parent, f.root.SecretID))
	f.bind("pp-job", `operator { policy = "write" }`, ns, parent.ID, "", "")

	dispatcher := f.token("dispatcher", ns, acl.NamespaceCapabilityDispatchJob)
	resp, err := f.dispatch(&structs.JobDispatchRequest{
		JobID:        parent.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: dispatcher.SecretID},
	})
	must.NoError(t, err)
	child := f.job(ns, resp.DispatchedJobID)
	must.True(t, child.IsPeriodicActive(), must.Sprint("the dispatched child is periodic"))
	must.Eq(t, "", resp.EvalID, must.Sprint("a periodic child gets no evaluation of its own"))

	forced, err := f.force(ns, child.ID, dispatcher.SecretID)
	must.NoError(t, err)
	eval, err := f.s.fsm.State().EvalByID(nil, forced.EvalID)
	must.NoError(t, err)
	grandchild := f.job(ns, eval.JobID)
	must.Eq(t, child.ID, grandchild.ParentID)
	must.True(t, strings.HasPrefix(grandchild.ID, child.ID+structs.PeriodicLaunchSuffix))

	claims, names := f.boundPolicies(grandchild)
	// CURRENT BEHAVIOUR: the identity names the dispatched child, and no
	// bound policy applies.
	must.Eq(t, child.ID, claims.JobID)
	must.SliceEmpty(t, names)
}

// A policy stays bound to a namespace and a job ID when the job is gone: a
// token with submit-job registers a new job of that ID and its allocations
// run under the policy. JOG-2 covers the first registration of a job ID too:
// a token without the bound policy is refused, a holder (root) may.
func TestJobBoundPolicies_NewJobOfBoundIDGetsPolicies(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newJobBoundFixture(t)
	defer cleanup()

	ns := structs.DefaultNamespace
	f.bind("gone-job", `operator { policy = "write" }`, ns, "gone", "", "")
	team := f.token("team", ns, acl.NamespaceCapabilitySubmitJob)

	job := mock.BatchJob()
	job.ID, job.Name = "gone", "gone"
	err := f.register(job, team.SecretID)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	must.ErrorContains(t, err, "gone-job")
	stored, err := f.s.fsm.State().JobByID(nil, ns, job.ID)
	must.NoError(t, err)
	must.Nil(t, stored, must.Sprint("the refused job is not registered"))

	must.NoError(t, f.register(job, f.root.SecretID))
	_, names := f.boundPolicies(f.job(ns, job.ID))
	must.Eq(t, []string{"gone-job"}, names)
}
