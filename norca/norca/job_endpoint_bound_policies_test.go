// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// boundPolicyFixture is a server with a job "ctrl" that has an ACL policy
// bound to it, a policy bound to the job ID "fresh" that no job has yet, and
// tokens that may submit jobs in the namespace: one without the bound
// policies, one that names "ctrl-policy" and one that has it through a role.
type boundPolicyFixture struct {
	t    *testing.T
	srv  *Server
	call func(method string, args, reply any) error

	root, team, holder, roleHolder *structs.ACLToken
	ctrl                           *structs.Job
}

func newBoundPolicyFixture(t *testing.T) (*boundPolicyFixture, func()) {
	s, root, cleanup := TestACLServer(t, nil)
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	store := s.fsm.State()
	f := &boundPolicyFixture{t: t, srv: s, root: root}
	f.call = func(method string, args, reply any) error {
		return msgpackrpc.CallWithCodec(codec, method, args, reply)
	}

	f.ctrl = mock.Job()
	f.ctrl.ID, f.ctrl.Name = "ctrl", "ctrl"
	must.NoError(t, f.register(f.ctrl.Copy(), root.SecretID))

	index, err := store.LatestIndex()
	must.NoError(t, err)
	bound := func(name, jobID string) *structs.ACLPolicy {
		p := &structs.ACLPolicy{
			Name:   name,
			Rules:  `namespace "*" { policy = "write" }`,
			JobACL: &structs.JobACL{Namespace: structs.DefaultNamespace, JobID: jobID},
		}
		p.SetHash()
		return p
	}
	must.NoError(t, store.UpsertACLPolicies(structs.MsgTypeTestSetup, index+1, []*structs.ACLPolicy{
		bound("ctrl-policy", "ctrl"), bound("fresh-policy", "fresh"), bound("cron-policy", "cron"),
	}))

	submit := mock.NamespacePolicy(structs.DefaultNamespace, "",
		[]string{acl.NamespaceCapabilitySubmitJob, acl.NamespaceCapabilityReadJob})
	f.team = mock.CreatePolicyAndToken(t, store, index+2, "team", submit)
	f.holder = mock.CreateToken(t, store, index+4, []string{"team", "ctrl-policy"})

	role := &structs.ACLRole{
		ID:       uuid.Generate(),
		Name:     "ctrl-role",
		Policies: []*structs.ACLRolePolicyLink{{Name: "ctrl-policy"}},
	}
	role.SetHash()
	must.NoError(t, store.UpsertACLRoles(structs.MsgTypeTestSetup, index+5, []*structs.ACLRole{role}, false))
	f.roleHolder = mock.ACLToken()
	f.roleHolder.Policies = []string{"team"}
	f.roleHolder.Roles = []*structs.ACLTokenRoleLink{{ID: role.ID}}
	f.roleHolder.SetHash()
	must.NoError(t, store.UpsertACLTokens(structs.MsgTypeTestSetup, index+6, []*structs.ACLToken{f.roleHolder}))

	return f, func() {
		_ = codec.Close()
		cleanup()
	}
}

func (f *boundPolicyFixture) register(job *structs.Job, token string) error {
	var resp structs.JobRegisterResponse
	return f.call("Job.Register", &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: token},
	}, &resp)
}

func (f *boundPolicyFixture) revert(jobID string, version uint64, token string) error {
	var resp structs.JobRegisterResponse
	return f.call("Job.Revert", &structs.JobRevertRequest{
		JobID:        jobID,
		JobVersion:   version,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &resp)
}

func (f *boundPolicyFixture) live(jobID string) *structs.Job {
	job, err := f.srv.State().JobByID(nil, structs.DefaultNamespace, jobID)
	must.NoError(f.t, err)
	return job
}

// changedCtrl returns the job "ctrl" with a task that runs something else.
func (f *boundPolicyFixture) changedCtrl(value string) *structs.Job {
	job := f.ctrl.Copy()
	job.TaskGroups[0].Tasks[0].Env["INJECTED"] = value
	return job
}

// An ACL policy bound to a job is granted to every allocation of the job,
// whoever registered it (review finding JOG-2). A write that changes what
// runs under the policies needs a token that holds each of them: by name,
// through a role, or a management token.
func TestJobEndpoint_Register_BoundPoliciesNeedTheirHolder(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newBoundPolicyFixture(t)
	defer cleanup()

	// A token that may only submit jobs can't give the job "ctrl" a task of
	// its own: it doesn't hold the policy bound to "ctrl".
	before := f.live("ctrl")
	err := f.register(f.changedCtrl("team"), f.team.SecretID)
	must.ErrorContains(t, err, "403,Permission denied")
	must.ErrorContains(t, err, `"ctrl-policy"`)
	after := f.live("ctrl")
	must.Eq(t, before.Version, after.Version)
	must.Eq(t, before.JobModifyIndex, after.JobModifyIndex)

	// The same job as it is changes nothing that runs: no right needed.
	must.NoError(t, f.register(f.ctrl.Copy(), f.team.SecretID))
	_, injected := f.live("ctrl").TaskGroups[0].Tasks[0].Env["INJECTED"]
	must.False(t, injected)

	// A token that names the policy, one that has it through a role, and a
	// management token may change it.
	must.NoError(t, f.register(f.changedCtrl("holder"), f.holder.SecretID))
	must.Eq(t, "holder", f.live("ctrl").TaskGroups[0].Tasks[0].Env["INJECTED"])
	must.NoError(t, f.register(f.changedCtrl("role"), f.roleHolder.SecretID))
	must.Eq(t, "role", f.live("ctrl").TaskGroups[0].Tasks[0].Env["INJECTED"])
	must.NoError(t, f.register(f.changedCtrl("root"), f.root.SecretID))
	must.Eq(t, "root", f.live("ctrl").TaskGroups[0].Tasks[0].Env["INJECTED"])

	// A job ID with a policy bound to it before the job exists: the first
	// registration needs the policy too.
	fresh := mock.Job()
	fresh.ID, fresh.Name = "fresh", "fresh"
	err = f.register(fresh.Copy(), f.team.SecretID)
	must.ErrorContains(t, err, "403,Permission denied")
	must.ErrorContains(t, err, `"fresh-policy"`)
	must.Nil(t, f.live("fresh"))
	err = f.register(fresh.Copy(), f.holder.SecretID)
	must.ErrorContains(t, err, `"fresh-policy"`)
	must.Nil(t, f.live("fresh"))
	must.NoError(t, f.register(fresh.Copy(), f.root.SecretID))
	must.NotNil(t, f.live("fresh"))

	// A job without bound policies needs none.
	other := mock.Job()
	other.ID, other.Name = "other", "other"
	must.NoError(t, f.register(other, f.team.SecretID))
}

// Job.Revert writes an earlier version of the tasks: the same rule.
func TestJobEndpoint_Revert_BoundPoliciesNeedTheirHolder(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newBoundPolicyFixture(t)
	defer cleanup()

	must.NoError(t, f.register(f.changedCtrl("v1"), f.root.SecretID))
	current := f.live("ctrl")
	must.Eq(t, uint64(1), current.Version)

	err := f.revert("ctrl", 0, f.team.SecretID)
	must.ErrorContains(t, err, "403,Permission denied")
	must.ErrorContains(t, err, `"ctrl-policy"`)
	must.Eq(t, current.JobModifyIndex, f.live("ctrl").JobModifyIndex)

	must.NoError(t, f.revert("ctrl", 0, f.holder.SecretID))
	must.Eq(t, uint64(2), f.live("ctrl").Version)
	_, injected := f.live("ctrl").TaskGroups[0].Tasks[0].Env["INJECTED"]
	must.False(t, injected)
	must.NoError(t, f.revert("ctrl", 1, f.root.SecretID))
}

// The paths that don't change the tasks keep working without the bound
// policies: a periodic child launched by the leader runs under the policy
// bound to its parent.
func TestJobEndpoint_BoundPolicies_PeriodicChildren(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newBoundPolicyFixture(t)
	defer cleanup()

	cron := mock.PeriodicJob()
	cron.ID, cron.Name = "cron", "cron"
	must.ErrorContains(t, f.register(cron.Copy(), f.team.SecretID), `"cron-policy"`)
	must.NoError(t, f.register(cron.Copy(), f.root.SecretID))

	var resp structs.PeriodicForceResponse
	must.NoError(t, f.call("Periodic.Force", &structs.PeriodicForceRequest{
		JobID:        cron.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: cron.Namespace, AuthToken: f.team.SecretID},
	}, &resp))
	children, err := f.srv.State().JobsByIDPrefix(nil, cron.Namespace, cron.ID+structs.PeriodicLaunchSuffix)
	must.NoError(t, err)
	child := children.Next()
	must.NotNil(t, child)
	must.Eq(t, cron.ID, child.(*structs.Job).ParentID)
}

// A workload identity holds the policies bound to its own job, group and
// task: a job it registers under an ID with other policies bound to it
// needs those as well. A client node holds none.
func TestJobEndpoint_BoundPolicies_HeldByWorkloadIdentity(t *testing.T) {
	ci.Parallel(t)
	srv, _, cleanup := TestACLServer(t, nil)
	defer cleanup()
	store := srv.fsm.State()

	alloc := mock.Alloc()
	claims := &structs.IdentityClaims{
		Namespace:    alloc.Namespace,
		JobID:        alloc.Job.ID,
		AllocationID: alloc.ID,
		TaskName:     alloc.Job.TaskGroups[0].Tasks[0].Name,
	}
	own := mock.ACLPolicy()
	own.Name = "own"
	own.JobACL = &structs.JobACL{Namespace: alloc.Namespace, JobID: alloc.Job.ID, Group: alloc.TaskGroup}
	own.SetHash()
	other := mock.ACLPolicy()
	other.Name = "other"
	other.JobACL = &structs.JobACL{Namespace: alloc.Namespace, JobID: "other"}
	other.SetHash()
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 100, []*structs.Allocation{alloc}))
	must.NoError(t, store.UpsertACLPolicies(structs.MsgTypeTestSetup, 101, []*structs.ACLPolicy{own, other}))

	snap, err := store.Snapshot()
	must.NoError(t, err)
	held, err := srv.heldPolicyNames(snap, &structs.AuthenticatedIdentity{Claims: claims})
	must.NoError(t, err)
	must.Eq(t, map[string]bool{"own": true}, held)

	held, err = srv.heldPolicyNames(snap, &structs.AuthenticatedIdentity{ClientID: "node"})
	must.NoError(t, err)
	must.MapEmpty(t, held)
}

// bind binds an ACL policy that may write every namespace to the job ID.
func (f *boundPolicyFixture) bind(name, jobID string) {
	store := f.srv.State()
	index, err := store.LatestIndex()
	must.NoError(f.t, err)
	p := &structs.ACLPolicy{
		Name:   name,
		Rules:  `namespace "*" { policy = "write" }`,
		JobACL: &structs.JobACL{Namespace: structs.DefaultNamespace, JobID: jobID},
	}
	p.SetHash()
	must.NoError(f.t, store.UpsertACLPolicies(structs.MsgTypeTestSetup, index+1, []*structs.ACLPolicy{p}))
}

// A new version of a dispatched or periodic child keeps its parent
// (checkJobParent) and runs with the policies bound to the parent: it needs
// them like the parent does.
func TestJobEndpoint_BoundPolicies_ChildNewVersion(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newBoundPolicyFixture(t)
	defer cleanup()

	disp := mock.BatchJob()
	disp.ID, disp.Name = "disp", "disp"
	disp.ParameterizedJob = &structs.ParameterizedJobConfig{}
	must.NoError(t, f.register(disp, f.root.SecretID))
	f.bind("disp-policy", disp.ID)
	var dispatched structs.JobDispatchResponse
	must.NoError(t, f.call("Job.Dispatch", &structs.JobDispatchRequest{
		JobID:        disp.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: disp.Namespace, AuthToken: f.root.SecretID},
	}, &dispatched))

	cron := mock.PeriodicJob()
	cron.ID, cron.Name = "cron", "cron"
	must.NoError(t, f.register(cron, f.root.SecretID))
	var forced structs.PeriodicForceResponse
	must.NoError(t, f.call("Periodic.Force", &structs.PeriodicForceRequest{
		JobID:        cron.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: cron.Namespace, AuthToken: f.team.SecretID},
	}, &forced))
	eval, err := f.srv.State().EvalByID(nil, forced.EvalID)
	must.NoError(t, err)

	for _, tc := range []struct {
		name, childID, parent, policy string
	}{
		{name: "dispatched child", childID: dispatched.DispatchedJobID, parent: disp.ID, policy: "disp-policy"},
		{name: "periodic child", childID: eval.JobID, parent: cron.ID, policy: "cron-policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := f.live(tc.childID)
			must.NotNil(t, child)
			must.Eq(t, tc.parent, child.ParentID)
			update := child.Copy()
			update.Payload = nil // a payload is only dispatched
			update.TaskGroups[0].Tasks[0].Config = map[string]interface{}{"run_for": "1h", "chosen_by": "team"}
			err := f.register(update.Copy(), f.team.SecretID)
			must.ErrorContains(t, err, "403,Permission denied")
			must.ErrorContains(t, err, `"`+tc.policy+`"`)
			must.ErrorContains(t, err, `parent job "`+tc.parent+`"`)
			must.Eq(t, child.JobModifyIndex, f.live(tc.childID).JobModifyIndex)
			must.NoError(t, f.register(update, f.root.SecretID))
			must.Eq(t, "team", f.live(tc.childID).TaskGroups[0].Tasks[0].Config["chosen_by"])
		})
	}
}

// Not only the tasks: the job and group meta reach the tasks through
// interpolation, and the datacenters and constraints decide the nodes the
// tasks run on. Any change of the job needs the bound policies.
func TestJobEndpoint_BoundPolicies_AnyChangeOfTheJob(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newBoundPolicyFixture(t)
	defer cleanup()

	for name, change := range map[string]func(*structs.Job){
		"job meta":    func(j *structs.Job) { j.Meta = map[string]string{"cmd": "evil"} },
		"group meta":  func(j *structs.Job) { j.TaskGroups[0].Meta = map[string]string{"cmd": "evil"} },
		"datacenters": func(j *structs.Job) { j.Datacenters = []string{"attacker"} },
		"constraint": func(j *structs.Job) {
			j.Constraints = append(j.Constraints, &structs.Constraint{
				LTarget: "${node.unique.name}", RTarget: "attacker", Operand: "="})
		},
		"stop": func(j *structs.Job) { j.Stop = true },
	} {
		t.Run(name, func(t *testing.T) {
			before := f.live("ctrl")
			job := f.ctrl.Copy()
			change(job)
			err := f.register(job, f.team.SecretID)
			must.ErrorContains(t, err, "403,Permission denied")
			must.ErrorContains(t, err, `"ctrl-policy"`)
			must.Eq(t, before.JobModifyIndex, f.live("ctrl").JobModifyIndex)
		})
	}
}

// The requests that steer the automatic revert of a job with bound ACL
// policies need a token that holds them (review finding JOG-17, option C):
// Job.Stable chooses the version a revert registers, Deployment.Fail and
// Deployment.SetAllocHealth start the revert. Before, submit-job was enough
// to pick a version from before the binding and fail the deployment of the
// policy holder. A job without bound policies keeps the old rule.
func TestJobEndpoint_BoundPolicies_SteerTheRevert(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newBoundPolicyFixture(t)
	defer cleanup()
	store := f.srv.fsm.State()

	free := mock.Job()
	must.NoError(t, f.register(free.Copy(), f.team.SecretID))

	index, err := store.LatestIndex()
	must.NoError(t, err)
	deployment := func(job *structs.Job, index uint64) *structs.Deployment {
		d := mock.Deployment()
		d.Namespace, d.JobID = job.Namespace, job.ID
		must.NoError(t, store.UpsertDeployment(index, d))
		return d
	}
	ctrlDeploy := deployment(f.ctrl, index+1)
	freeDeploy := deployment(free, index+2)

	write := func(token string) structs.WriteRequest {
		return structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token}
	}
	stable := func(jobID, token string) error {
		var resp structs.JobStabilityResponse
		return f.call("Job.Stable", &structs.JobStabilityRequest{
			JobID: jobID, JobVersion: 0, Stable: true, WriteRequest: write(token),
		}, &resp)
	}
	fail := func(d *structs.Deployment, token string) error {
		var resp structs.DeploymentUpdateResponse
		return f.call("Deployment.Fail", &structs.DeploymentFailRequest{
			DeploymentID: d.ID, WriteRequest: write(token),
		}, &resp)
	}
	health := func(d *structs.Deployment, token string) error {
		var resp structs.DeploymentUpdateResponse
		return f.call("Deployment.SetAllocHealth", &structs.DeploymentAllocHealthRequest{
			DeploymentID: d.ID, UnhealthyAllocationIDs: []string{uuid.Generate()}, WriteRequest: write(token),
		}, &resp)
	}
	notDenied := func(err error) {
		t.Helper()
		if err != nil {
			must.StrNotContains(t, err.Error(), "Permission denied")
		}
	}

	for name, call := range map[string]func(token string) error{
		"Job.Stable":                func(token string) error { return stable("ctrl", token) },
		"Deployment.Fail":           func(token string) error { return fail(ctrlDeploy, token) },
		"Deployment.SetAllocHealth": func(token string) error { return health(ctrlDeploy, token) },
	} {
		err := call(f.team.SecretID)
		must.ErrorContains(t, err, "403,Permission denied", must.Sprint(name))
		must.ErrorContains(t, err, `"ctrl-policy"`, must.Sprint(name))
	}

	notDenied(stable("ctrl", f.holder.SecretID))
	notDenied(stable("ctrl", f.roleHolder.SecretID))
	notDenied(health(ctrlDeploy, f.holder.SecretID))
	notDenied(fail(ctrlDeploy, f.root.SecretID))

	// a job without bound policies: submit-job is enough, as before
	notDenied(stable(free.ID, f.team.SecretID))
	notDenied(health(freeDeploy, f.team.SecretID))
	notDenied(fail(freeDeploy, f.team.SecretID))
}
