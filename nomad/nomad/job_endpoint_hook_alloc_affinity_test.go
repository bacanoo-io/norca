// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func allocAffinityTestJob(namespace string) *structs.Job {
	job := mock.Job()
	job.TaskGroups[0].Colocate = []*structs.AllocAffinity{{
		Selector:  map[string]string{"app": "api"},
		Namespace: namespace,
		Scope:     "${node.unique.id}",
		Weight:    50,
	}}
	return job
}

func TestJobEndpoint_Register_AllocAffinity(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	// A rule referring to a missing namespace registers with a warning.
	job := allocAffinityTestJob("missing")
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
	must.StrContains(t, resp.Warnings, `nonexistent namespace "missing"`)

	// The rules are stored.
	out, err := s1.fsm.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, job.TaskGroups[0].Colocate, out.TaskGroups[0].Colocate)

	// An invalid rule is rejected.
	job2 := allocAffinityTestJob("")
	job2.TaskGroups[0].Colocate[0].Scope = "rack"
	req.Job = job2
	err = msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
	must.ErrorContains(t, err, "Invalid scope")
}

func TestJobEndpoint_Register_AllocAffinity_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	ns := mock.Namespace()
	ns.Name = "other"
	must.NoError(t, s1.fsm.State().UpsertNamespaces(1000, []*structs.Namespace{ns}))

	submitPolicy := mock.NamespacePolicy(structs.DefaultNamespace, "",
		[]string{acl.NamespaceCapabilityReadJob, acl.NamespaceCapabilitySubmitJob})
	submitToken := mock.CreatePolicyAndToken(t, s1.State(), 1001, "submit", submitPolicy)
	readOtherPolicy := mock.NamespacePolicy("other", "", []string{acl.NamespaceCapabilityReadJob})
	readOtherToken := mock.CreatePolicyAndToken(t, s1.State(), 1002, "submit-read-other", submitPolicy+readOtherPolicy)

	cases := []struct {
		name      string
		namespace string
		token     string
		denied    bool
	}{
		{name: "own namespace", namespace: "", token: submitToken.SecretID},
		{name: "other namespace without read", namespace: "other", token: submitToken.SecretID, denied: true},
		{name: "other namespace with read", namespace: "other", token: readOtherToken.SecretID},
		{name: "management", namespace: "other", token: root.SecretID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := allocAffinityTestJob(tc.namespace)
			wr := structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: tc.token}

			var resp structs.JobRegisterResponse
			err := msgpackrpc.CallWithCodec(codec, "Job.Register",
				&structs.JobRegisterRequest{Job: job, WriteRequest: wr}, &resp)
			var planResp structs.JobPlanResponse
			planErr := msgpackrpc.CallWithCodec(codec, "Job.Plan",
				&structs.JobPlanRequest{Job: job, WriteRequest: wr}, &planResp)
			if tc.denied {
				must.EqError(t, err, structs.ErrPermissionDenied.Error())
				must.EqError(t, planErr, structs.ErrPermissionDenied.Error())
			} else {
				must.NoError(t, err)
				must.NoError(t, planErr)
			}
		})
	}
}
