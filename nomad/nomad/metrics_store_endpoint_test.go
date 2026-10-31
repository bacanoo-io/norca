// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/hashicorp/nomad/nomad/metricsstore"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"
)

// enableMetricsStore returns a configuration callback that turns the bus
// and the metrics store on, with a one second sample interval.
func enableMetricsStore(t *testing.T) func(*Config) {
	return func(c *Config) {
		enableBus(t)(c)
		c.MetricsStore = metricsstore.DefaultConfig()
		c.MetricsStore.Dir = t.TempDir()
		c.MetricsStore.SampleInterval = time.Second
		c.MetricsStore.PartitionDuration = time.Hour
		c.MetricsStore.Retention = 2 * time.Hour
	}
}

// TestMetricsStore_History checks that the usage published on the bus is
// stored and returned by the history, with the ACL of the namespace, and
// that the clients are asked for usage without the autoscaler.
func TestMetricsStore_History(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := TestACLServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	codec := rpcClient(t, s)
	st := s.fsm.State()

	node := mock.Node()
	var regResp structs.NodeUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.Register", &structs.NodeRegisterRequest{
		Node:         node,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID},
	}, &regResp))
	must.Eq(t, autoscaler.DefaultClientMetricsInterval, regResp.TaskGroupUsageInterval)

	js := busJS(t, s)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), "NORCA_TELEMETRY_global")
		return err == nil, err
	})
	nc, err := nats.Connect(busURL(s), nats.UserInfo(node.ID, node.SecretID))
	must.NoError(t, err)
	defer nc.Close()
	report, err := json.Marshal(&structs.TaskGroupUsageReport{NodeID: node.ID, Window: 10 * time.Second,
		Groups: []*structs.TaskGroupUsage{{
			Namespace: "default", JobID: "api", TaskGroup: "web", Allocs: 1, Samples: 5,
			CPUPercent: structs.TaskGroupUsageValue{Avg: 25, Max: 40},
		}}})
	must.NoError(t, err)

	history := func(token, namespace string) (*structs.MetricsHistoryResponse, error) {
		var resp structs.MetricsHistoryResponse
		err := msgpackrpc.CallWithCodec(codec, structs.MetricsStoreHistoryRPCMethod, &structs.MetricsHistoryRequest{
			JobID: "api", Group: "web", Metric: metricsstore.MetricCPU,
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: namespace, AuthToken: token},
		}, &resp)
		return &resp, err
	}
	waitBus(t, func() (bool, error) {
		must.NoError(t, nc.Publish(structs.BusTelemetrySubject("global", node.ID), report))
		resp, err := history(root.SecretID, "default")
		if err != nil {
			return false, err
		}
		return len(resp.Points) > 0, nil
	})
	resp, err := history(root.SecretID, "default")
	must.NoError(t, err)
	must.True(t, resp.Enabled)
	must.Eq(t, 25.0, resp.Points[0].Avg)
	must.Eq(t, 40.0, resp.Points[0].Max)

	// read-job in the namespace is required.
	policy := func(name, rules string) *structs.ACLPolicy {
		p := &structs.ACLPolicy{Name: name, Rules: rules}
		p.SetHash()
		return p
	}
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 1000, []*structs.ACLPolicy{
		policy("reader", `namespace "default" { policy = "read" }`),
		policy("other", `namespace "other" { policy = "read" }`),
	}))
	token := func(index uint64, policies ...string) string {
		tok := &structs.ACLToken{AccessorID: uuid.Generate(), SecretID: uuid.Generate(), Type: structs.ACLClientToken, Policies: policies}
		tok.SetHash()
		must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, index, []*structs.ACLToken{tok}))
		return tok.SecretID
	}
	reader, other := token(1001, "reader"), token(1002, "other")
	resp, err = history(reader, "default")
	must.NoError(t, err)
	must.SliceNotEmpty(t, resp.Points)
	_, err = history(other, "default")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	resp, err = history(other, "other")
	must.NoError(t, err)
	must.SliceEmpty(t, resp.Points)

	// The state of the store needs operator read.
	var status structs.MetricsStoreStatusResponse
	must.ErrorContains(t, msgpackrpc.CallWithCodec(codec, structs.MetricsStoreStatusRPCMethod, &structs.MetricsStoreStatusRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: reader},
	}, &status), structs.ErrPermissionDenied.Error())
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.MetricsStoreStatusRPCMethod, &structs.MetricsStoreStatusRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID},
	}, &status))
	must.True(t, status.Status.Enabled)
	must.Eq(t, 18, status.Status.Series)
	must.False(t, status.Status.Newest.IsZero())

	// The servers sample the running and the queued allocations from the
	// job summaries.
	must.NoError(t, st.UpsertJobSummary(1100, &structs.JobSummary{
		JobID: "batch", Namespace: "default",
		Summary: map[string]structs.TaskGroupSummary{"work": {Running: 2, Queued: 3}},
	}))
	waitBus(t, func() (bool, error) {
		var resp structs.MetricsHistoryResponse
		err := msgpackrpc.CallWithCodec(codec, structs.MetricsStoreHistoryRPCMethod, &structs.MetricsHistoryRequest{
			JobID: "batch", Group: "work", Metric: metricsstore.MetricAllocsQueued,
			QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID},
		}, &resp)
		return err == nil && len(resp.Points) > 0 && resp.Points[0].Avg == 3, err
	})
}

// TestMetricsStore_Disabled checks the answers of a server without the
// store.
func TestMetricsStore_Disabled(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	codec := rpcClient(t, s)

	var resp structs.MetricsHistoryResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.MetricsStoreHistoryRPCMethod, &structs.MetricsHistoryRequest{
		JobID: "api", Group: "web", QueryOptions: structs.QueryOptions{Region: "global"},
	}, &resp))
	must.False(t, resp.Enabled)
	must.SliceEmpty(t, resp.Points)

	err := msgpackrpc.CallWithCodec(codec, structs.MetricsStoreHistoryRPCMethod, &structs.MetricsHistoryRequest{
		QueryOptions: structs.QueryOptions{Region: "global"},
	}, &resp)
	must.ErrorContains(t, err, "job and group are required")

	var status structs.MetricsStoreStatusResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.MetricsStoreStatusRPCMethod, &structs.MetricsStoreStatusRequest{
		QueryOptions: structs.QueryOptions{Region: "global"},
	}, &status))
	must.False(t, status.Status.Enabled)
	must.NotEq(t, "", status.Status.Server)
}

// TestMetricsStore_SeedAutoscaler checks that a new leader fills the usage
// window of the built-in autoscaler from its metrics store.
func TestMetricsStore_SeedAutoscaler(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, func(c *Config) {
		enableMetricsStore(t)(c)
		c.Autoscaler = autoscaler.DefaultConfig()
		c.Autoscaler.Enabled = true
	})
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	at := time.Now().Add(-time.Minute)
	s.metricsStore.Ingest(&structs.TaskGroupUsageReport{NodeID: "n", Window: 10 * time.Second,
		Groups: []*structs.TaskGroupUsage{{
			Namespace: "default", JobID: "api", TaskGroup: "web", Allocs: 1, Samples: 5,
			CPUPercent: structs.TaskGroupUsageValue{Avg: 25, Max: 40},
		}}}, at)
	must.Eq(t, 0, s.autoscalerUsageGroupsForTest())
	s.seedAutoscalerUsage()
	must.Eq(t, 1, s.autoscalerUsageGroupsForTest())
}
