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

// TestRPC_ConsistentRead checks the consistent read mode: a follower always
// forwards it to the leader (also with AllowStale), the leader serves it only
// after confirming its leadership with a quorum, and a leader that lost its
// quorum refuses it instead of serving its possibly stale state.
func TestRPC_ConsistentRead(t *testing.T) {
	ci.Parallel(t)

	servers := make([]*Server, 3)
	for i := range servers {
		s, cleanup := TestServer(t, func(c *Config) {
			c.BootstrapExpect = 3
			c.RPCHoldTimeout = 100 * time.Millisecond
		})
		t.Cleanup(cleanup)
		servers[i] = s
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}

	var leader *Server
	var followers []*Server
	for _, s := range servers {
		if s.IsLeader() {
			leader = s
		} else {
			followers = append(followers, s)
		}
	}
	must.NotNil(t, leader)
	must.Len(t, 2, followers)

	// A job written through the leader is visible to a consistent read
	// served for a follower's client.
	job := mock.Job()
	reg := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, leader), "Job.Register", reg, &regResp))

	list := &structs.JobListRequest{QueryOptions: structs.QueryOptions{
		Region: "global", Namespace: job.Namespace, Consistent: true, AllowStale: true,
	}}
	var listResp structs.JobListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, followers[0]), "Job.List", list, &listResp))
	must.Len(t, 1, listResp.Jobs)
	must.Eq(t, job.ID, listResp.Jobs[0].ID)
	must.GreaterEq(t, regResp.JobModifyIndex, listResp.Index)

	// A stale read is served by the follower; Consistent overrides
	// AllowStale, so the follower forwards it to the leader.
	staleReq := &structs.JobListRequest{QueryOptions: structs.QueryOptions{
		Region: "global", Namespace: job.Namespace, AllowStale: true,
	}}
	var out structs.JobListResponse
	forwarded, err := followers[0].forward("Job.List", staleReq, staleReq, &out)
	must.NoError(t, err)
	must.False(t, forwarded)

	consistentReq := &structs.JobListRequest{QueryOptions: structs.QueryOptions{
		Region: "global", Namespace: job.Namespace, AllowStale: true, Consistent: true,
	}}
	must.False(t, consistentReq.AllowStaleRead())
	forwarded, err = followers[0].forward("Job.List", consistentReq, consistentReq, &out)
	must.NoError(t, err)
	must.True(t, forwarded)
	must.Len(t, 1, out.Jobs)

	// The leader confirms its leadership; a follower cannot.
	must.NoError(t, leader.verifyLeaderForRead())
	must.Error(t, followers[0].verifyLeaderForRead())

	// Without its quorum the leader refuses consistent reads.
	for _, f := range followers {
		must.NoError(t, f.Shutdown())
	}
	done := make(chan error, 1)
	go func() { done <- leader.verifyLeaderForRead() }()
	select {
	case err := <-done:
		must.Error(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("consistent read of a leader without quorum did not fail")
	}
}
