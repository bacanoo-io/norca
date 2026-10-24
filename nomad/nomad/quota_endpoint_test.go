// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/rpc"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func mockQuotaSpec() *structs.QuotaSpec {
	return &structs.QuotaSpec{
		Name:        "quota-" + uuid.Short(),
		Description: "test quota",
		Limits: []*structs.QuotaLimit{{
			Region:      "global",
			RegionLimit: &structs.Resources{CPU: 1000, MemoryMB: 1024},
		}},
	}
}

func upsertQuotaRPC(t *testing.T, codec rpc.ClientCodec, token string, specs ...*structs.QuotaSpec) error {
	req := &structs.QuotaSpecUpsertRequest{
		Quotas:       specs,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token},
	}
	var resp structs.GenericResponse
	return msgpackrpc.CallWithCodec(codec, "Quota.UpsertQuotaSpecs", req, &resp)
}

func TestQuotaEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	// Invalid specs are rejected
	bad := mockQuotaSpec()
	bad.Name = "*"
	must.ErrorContains(t, upsertQuotaRPC(t, codec, "", bad), "invalid quota specification")

	q1, q2 := mockQuotaSpec(), mockQuotaSpec()
	q1.Name = "aaa-" + q1.Name
	q2.Name = "bbb-" + q2.Name
	must.NoError(t, upsertQuotaRPC(t, codec, "", q1, q2))

	// List and prefix list
	listReq := &structs.QuotaSpecListRequest{QueryOptions: structs.QueryOptions{Region: "global"}}
	var listResp structs.QuotaSpecListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.ListQuotaSpecs", listReq, &listResp))
	must.Len(t, 2, listResp.Quotas)
	must.Positive(t, listResp.Index)

	listReq.Prefix = "bbb"
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.ListQuotaSpecs", listReq, &listResp))
	must.Len(t, 1, listResp.Quotas)
	must.Eq(t, q2.Name, listResp.Quotas[0].Name)

	// Get sets the defaults and hashes
	getReq := &structs.QuotaSpecSpecificRequest{Name: q1.Name, QueryOptions: structs.QueryOptions{Region: "global"}}
	var getResp structs.SingleQuotaSpecResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.GetQuotaSpec", getReq, &getResp))
	must.NotNil(t, getResp.Quota)
	must.Eq(t, structs.QuotaEnforceSchedule, getResp.Quota.Enforce)
	must.Len(t, 32, getResp.Quota.Hash)
	must.Len(t, 32, getResp.Quota.Limits[0].Hash)

	// Usage is keyed by the limit hash
	var usageResp structs.SingleQuotaUsageResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.GetQuotaUsage", getReq, &usageResp))
	must.NotNil(t, usageResp.Usage)
	entry := usageResp.Usage.Used[getResp.Quota.Limits[0].HashKey()]
	must.NotNil(t, entry)
	must.Eq(t, 0, *entry.Allocations)
	must.Eq(t, 0, *entry.VariablesLimit)

	var usagesResp structs.QuotaUsageListResponse
	listReq.Prefix = ""
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.ListQuotaUsages", listReq, &usagesResp))
	must.Len(t, 2, usagesResp.Usages)

	// Missing quota
	getReq.Name = "missing"
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.GetQuotaSpec", getReq, &getResp))
	must.Nil(t, getResp.Quota)

	// A quota used by a namespace can't be deleted
	ns := mock.Namespace()
	ns.Quota = q1.Name
	must.NoError(t, s1.fsm.State().UpsertNamespaces(1000, []*structs.Namespace{ns}))

	delReq := &structs.QuotaSpecDeleteRequest{Names: []string{q1.Name}, WriteRequest: structs.WriteRequest{Region: "global"}}
	var delResp structs.GenericResponse
	must.ErrorContains(t, msgpackrpc.CallWithCodec(codec, "Quota.DeleteQuotaSpecs", delReq, &delResp), "in use")

	delReq.Names = []string{q2.Name}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.DeleteQuotaSpecs", delReq, &delResp))
	out, err := s1.fsm.State().QuotaSpecByName(nil, q2.Name)
	must.NoError(t, err)
	must.Nil(t, out)
}

func TestQuotaEndpoint_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	state := s1.fsm.State()
	readToken := mock.CreatePolicyAndToken(t, state, 1001, "quota-read", `quota { policy = "read" }`)
	writeToken := mock.CreatePolicyAndToken(t, state, 1003, "quota-write", `quota { policy = "write" }`)

	q := mockQuotaSpec()

	// Anonymous and read tokens can't write
	must.EqError(t, upsertQuotaRPC(t, codec, "", q), structs.ErrPermissionDenied.Error())
	must.EqError(t, upsertQuotaRPC(t, codec, readToken.SecretID, q), structs.ErrPermissionDenied.Error())
	must.NoError(t, upsertQuotaRPC(t, codec, writeToken.SecretID, q))

	listReq := &structs.QuotaSpecListRequest{QueryOptions: structs.QueryOptions{Region: "global"}}
	var listResp structs.QuotaSpecListResponse
	must.EqError(t, msgpackrpc.CallWithCodec(codec, "Quota.ListQuotaSpecs", listReq, &listResp),
		structs.ErrPermissionDenied.Error())

	listReq.AuthToken = readToken.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.ListQuotaSpecs", listReq, &listResp))
	must.Len(t, 1, listResp.Quotas)

	getReq := &structs.QuotaSpecSpecificRequest{Name: q.Name, QueryOptions: structs.QueryOptions{Region: "global"}}
	var usageResp structs.SingleQuotaUsageResponse
	must.EqError(t, msgpackrpc.CallWithCodec(codec, "Quota.GetQuotaUsage", getReq, &usageResp),
		structs.ErrPermissionDenied.Error())
	getReq.AuthToken = root.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.GetQuotaUsage", getReq, &usageResp))

	delReq := &structs.QuotaSpecDeleteRequest{Names: []string{q.Name},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: readToken.SecretID}}
	var delResp structs.GenericResponse
	must.EqError(t, msgpackrpc.CallWithCodec(codec, "Quota.DeleteQuotaSpecs", delReq, &delResp),
		structs.ErrPermissionDenied.Error())
	delReq.AuthToken = writeToken.SecretID
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Quota.DeleteQuotaSpecs", delReq, &delResp))
}

func TestQuotaEndpoint_JobAdmission(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	q := mockQuotaSpec()
	q.TaskDefaults = &structs.Resources{CPU: 250, MemoryMB: 128}
	q.TaskMax = &structs.Resources{CPU: 400, MemoryMB: 512}
	must.NoError(t, upsertQuotaRPC(t, codec, "", q))

	ns := mock.Namespace()
	ns.Quota = q.Name
	must.NoError(t, s1.fsm.State().UpsertNamespaces(1000, []*structs.Namespace{ns}))

	register := func(job *structs.Job) error {
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		return msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
	}

	// A task above task_max is rejected
	job := mock.Job()
	job.Namespace = ns.Name
	must.ErrorContains(t, register(job), "exceeds the task_max")

	// Unset (built-in default) resources are filled in from task_defaults
	job.TaskGroups[0].Tasks[0].Resources.CPU = structs.DefaultResources().CPU
	job.TaskGroups[0].Tasks[0].Resources.MemoryMB = 0
	must.NoError(t, register(job))
	out, err := s1.fsm.State().JobByID(nil, ns.Name, job.ID)
	must.NoError(t, err)
	must.Eq(t, 250, out.TaskGroups[0].Tasks[0].Resources.CPU)
	must.Eq(t, 128, out.TaskGroups[0].Tasks[0].Resources.MemoryMB)

	// Explicit values are kept
	job2 := mock.Job()
	job2.Namespace = ns.Name
	job2.TaskGroups[0].Tasks[0].Resources.CPU = 300
	must.NoError(t, register(job2))
	out, err = s1.fsm.State().JobByID(nil, ns.Name, job2.ID)
	must.NoError(t, err)
	must.Eq(t, 300, out.TaskGroups[0].Tasks[0].Resources.CPU)

	// With enforce = "submit", a job whose total demand exceeds the limit is
	// rejected: 10 x 300 MHz > 1000 MHz.
	q2 := q.Copy()
	q2.Enforce = structs.QuotaEnforceSubmit
	must.NoError(t, upsertQuotaRPC(t, codec, "", q2))
	job3 := mock.Job()
	job3.Namespace = ns.Name
	job3.TaskGroups[0].Tasks[0].Resources.CPU = 300
	must.ErrorContains(t, register(job3), "job demand exceeds quota")

	job3.TaskGroups[0].Count = 3
	must.NoError(t, register(job3))
}

// TestQuota_Integration_BlockAndUnblock registers two jobs that together
// exceed the quota: only the allowed number of allocations is placed, the
// rest is blocked on the quota and placed once the quota is raised.
func TestQuota_Integration_BlockAndUnblock(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	state := s1.fsm.State()
	for i := 0; i < 3; i++ {
		must.NoError(t, state.UpsertNode(structs.MsgTypeTestSetup, uint64(100+i), mock.Node()))
	}

	// 3 allocations allowed
	q := mockQuotaSpec()
	q.Limits[0].RegionLimit = &structs.Resources{}
	q.Limits[0].Allocations = pointer.Of(3)
	must.NoError(t, upsertQuotaRPC(t, codec, "", q))

	ns := mock.Namespace()
	ns.Quota = q.Name
	must.NoError(t, state.UpsertNamespaces(200, []*structs.Namespace{ns}))

	var jobs []*structs.Job
	for i := 0; i < 2; i++ {
		job := mock.Job()
		job.Namespace = ns.Name
		job.TaskGroups[0].Count = 2
		job.TaskGroups[0].Tasks[0].Services = nil
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
		jobs = append(jobs, job)
	}

	countAllocs := func() int {
		n := 0
		for _, job := range jobs {
			allocs, err := state.AllocsByJob(nil, job.Namespace, job.ID, false)
			must.NoError(t, err)
			for _, a := range allocs {
				if !a.TerminalStatus() {
					n++
				}
			}
		}
		return n
	}

	// Exactly up to the limit is placed and the rest is blocked on the quota
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			return countAllocs() == 3 && s1.blockedEvals.Stats().TotalQuotaLimit == 1
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(50*time.Millisecond),
	))
	used := quotaUsedForTest(t, s1, q.Name)
	must.Eq(t, int64(3), used.Allocations)

	// Stay at the limit
	time.Sleep(200 * time.Millisecond)
	must.Eq(t, 3, countAllocs())

	// Raising the quota unblocks the blocked evaluation
	q2 := q.Copy()
	q2.Limits[0].Allocations = pointer.Of(10)
	must.NoError(t, upsertQuotaRPC(t, codec, "", q2))

	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			if n := countAllocs(); n != 4 {
				return fmt.Errorf("expected 4 allocs, got %d", n)
			}
			if n := s1.blockedEvals.Stats().TotalQuotaLimit; n != 0 {
				return fmt.Errorf("expected no quota blocked evals, got %d", n)
			}
			return nil
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(50*time.Millisecond),
	))
	must.Eq(t, int64(4), quotaUsedForTest(t, s1, q.Name).Allocations)
}

func quotaUsedForTest(t *testing.T, s *Server, name string) structs.QuotaResources {
	usage, err := s.fsm.State().QuotaUsageByName(nil, name)
	must.NoError(t, err)
	entry := usage.RegionUsage("global")
	must.NotNil(t, entry)
	return entry.UsageResources()
}
