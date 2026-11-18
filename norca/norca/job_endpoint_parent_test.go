// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// The parent of a job decides its workload identity and the ACL policies
// bound to it: only the servers set it, when they dispatch or launch a child
// (review finding JOG-7). A caller that sets it in Job.Register is refused;
// a new version of a child keeps the parent it has.
func TestJobEndpoint_Register_ParentIDSetByServersOnly(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := TestACLServer(t, nil)
	defer cleanup()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	store := s.fsm.State()

	register := func(job *structs.Job, token string) error {
		var resp structs.JobRegisterResponse
		return msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: token},
		}, &resp)
	}

	// A controller with a policy bound to it, and a team that may submit
	// jobs in its namespace.
	ctrl := mock.BatchJob()
	ctrl.ID, ctrl.Name = "ctrl", "ctrl"
	ctrl.ParameterizedJob = &structs.ParameterizedJobConfig{}
	must.NoError(t, register(ctrl, root.SecretID))
	policy := mock.ACLPolicy()
	policy.Name = "ctrl-policy"
	policy.Rules = `operator { policy = "write" }`
	policy.JobACL = &structs.JobACL{Namespace: ctrl.Namespace, JobID: ctrl.ID}
	policy.SetHash()
	index, err := store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.UpsertACLPolicies(structs.MsgTypeTestSetup, index+1, []*structs.ACLPolicy{policy}))
	team := mock.CreatePolicyAndToken(t, store, index+2, "team",
		mock.NamespacePolicy(ctrl.Namespace, "", []string{acl.NamespaceCapabilitySubmitJob, acl.NamespaceCapabilityDispatchJob}))

	// A job that names the controller as its parent is refused.
	evil := mock.BatchJob()
	evil.ID, evil.Name = "evil", "evil"
	evil.ParentID = ctrl.ID
	err = register(evil, team.SecretID)
	must.ErrorContains(t, err, "parent")
	got, err := store.JobByID(nil, evil.Namespace, evil.ID)
	must.NoError(t, err)
	must.Nil(t, got, must.Sprint("a job with a parent of the caller's choice was registered"))

	// Also for a management token: only the servers set it.
	must.ErrorContains(t, register(evil.Copy(), root.SecretID), "parent")

	// A child dispatched by the servers can get a new version with its
	// parent, but not with another one.
	var dispatched structs.JobDispatchResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Dispatch", &structs.JobDispatchRequest{
		JobID:        ctrl.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ctrl.Namespace, AuthToken: team.SecretID},
	}, &dispatched))
	child, err := store.JobByID(nil, ctrl.Namespace, dispatched.DispatchedJobID)
	must.NoError(t, err)
	must.Eq(t, ctrl.ID, child.ParentID)
	update := child.Copy()
	update.Meta = map[string]string{"changed": "yes"}
	update.Payload = nil // a payload is only dispatched
	must.NoError(t, register(update, team.SecretID))
	moved := child.Copy()
	moved.ParentID, moved.Payload = "other", nil
	must.ErrorContains(t, register(moved, team.SecretID), "parent")
}
