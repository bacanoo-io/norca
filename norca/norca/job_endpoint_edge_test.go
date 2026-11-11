// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
)

// edgeTestCluster starts a leader and a follower and returns them.
func edgeTestCluster(t *testing.T) (leader, follower *Server) {
	t.Helper()
	s1, cleanup1 := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	t.Cleanup(cleanup1)
	testutil.WaitForLeader(t, s1.RPC)
	s2, cleanup2 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
	})
	t.Cleanup(cleanup2)
	TestJoin(t, s1, s2)
	waitForSuffrage(t, s1, map[*Server]raft.ServerSuffrage{s2: raft.Voter})
	testutil.WaitForLeader(t, s2.RPC)
	return s1, s2
}

func TestJob_EdgeValidateRegister(t *testing.T) {
	ci.Parallel(t)

	_, s2 := edgeTestCluster(t)
	endpoint := NewJobEndpoints(s2, nil)

	register := func(job *structs.Job) *structs.JobRegisterRequest {
		return &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
	}
	must.True(t, s2.receivesForLeader(register(mock.Job())))

	// A valid job passes; the request is not changed.
	job := mock.Job()
	args := register(job)
	must.NoError(t, endpoint.edgeValidateRegister(args))
	must.Eq(t, job, args.Job)

	// An invalid job is rejected with the error of the job validation.
	bad := mock.Job()
	bad.TaskGroups = nil
	err := endpoint.edgeValidateRegister(register(bad))
	must.ErrorContains(t, err, "Missing job task groups")

	bad = mock.Job()
	bad.Priority = 1000
	must.ErrorContains(t, endpoint.edgeValidateRegister(register(bad)), "job priority must be between")

	// A forwarded request or one of another region is not checked again.
	args = register(mock.Job())
	args.SetForwarded()
	must.False(t, s2.receivesForLeader(args))
	args = register(mock.Job())
	args.Region = "other"
	must.False(t, s2.receivesForLeader(args))
}

func TestJob_Register_EdgeAndCapacity(t *testing.T) {
	ci.Parallel(t)

	_, s2 := edgeTestCluster(t)

	// An invalid job sent to the follower is rejected there, with the
	// error the leader returns.
	bad := mock.Job()
	bad.TaskGroups = nil
	var reply structs.JobRegisterResponse
	err := s2.RPC("Job.Register", &structs.JobRegisterRequest{
		Job:          bad,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: bad.Namespace},
	}, &reply)
	must.ErrorContains(t, err, "Missing job task groups")

	// A valid job with the dry run: the follower reports the groups that
	// can't be placed (no client), the leader registers the job.
	job := mock.Job()
	reply = structs.JobRegisterResponse{}
	must.NoError(t, s2.RPC("Job.Register", &structs.JobRegisterRequest{
		Job:            job,
		DryRunCapacity: true,
		WriteRequest:   structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}, &reply))
	must.NotEq(t, "", reply.EvalID)
	must.NotNil(t, reply.CapacityCheck)
	must.Eq(t, s2.config.NodeName, reply.CapacityCheck.Server)
	must.Eq(t, "", reply.CapacityCheck.Error)
	must.MapContainsKey(t, reply.CapacityCheck.FailedTGAllocs, job.TaskGroups[0].Name)

	// Without the dry run there is no capacity check.
	job2 := mock.Job()
	reply = structs.JobRegisterResponse{}
	must.NoError(t, s2.RPC("Job.Register", &structs.JobRegisterRequest{
		Job:          job2,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job2.Namespace},
	}, &reply))
	must.Nil(t, reply.CapacityCheck)

	// A plan with the dry run runs on the follower.
	var planReply structs.JobPlanResponse
	must.NoError(t, s2.RPC("Job.Plan", &structs.JobPlanRequest{
		Job:            mock.Job(),
		Diff:           true,
		DryRunCapacity: true,
		WriteRequest:   structs.WriteRequest{Region: "global", Namespace: "default"},
	}, &planReply))
	must.NotNil(t, planReply.Annotations)
	must.MapNotEmpty(t, planReply.FailedTGAllocs)
}
