// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// TestMetricsStore_RestartSampler checks the task restarts per task group as
// their increase between two samples: the first sample takes what happened
// before, an allocation removed by the garbage collection makes no negative
// increase, a new allocation counts all its restarts.
func TestMetricsStore_RestartSampler(t *testing.T) {
	ci.Parallel(t)
	st := state.TestStateStore(t)

	withRestarts := func(alloc *structs.Allocation, restarts ...uint64) *structs.Allocation {
		alloc = alloc.Copy()
		alloc.TaskStates = map[string]*structs.TaskState{}
		for i, n := range restarts {
			alloc.TaskStates[string(rune('a'+i))] = &structs.TaskState{State: structs.TaskStateRunning, Restarts: n}
		}
		return alloc
	}
	a1, a2 := mock.Alloc(), mock.Alloc()
	a2.JobID, a2.Job = a1.JobID, a1.Job
	index := uint64(1000)
	update := func(allocs ...*structs.Allocation) {
		index++
		must.NoError(t, st.UpdateAllocsFromClient(structs.MsgTypeTestSetup, index, allocs))
	}
	index++
	must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, index, []*structs.Allocation{a1, a2}))
	update(withRestarts(a1, 2, 1), withRestarts(a2, 4))

	var r restartSampler
	sample := func() map[metricsstore.GroupKey]int {
		snap, err := st.Snapshot()
		must.NoError(t, err)
		out, err := r.increase(snap)
		must.NoError(t, err)
		return out
	}
	key := metricsstore.GroupKey{Namespace: a1.Namespace, Job: a1.JobID, Group: a1.TaskGroup}

	// The restarts before the first sample are not an increase.
	must.MapEmpty(t, sample())

	update(withRestarts(a1, 3, 2), withRestarts(a2, 5))
	must.Eq(t, map[metricsstore.GroupKey]int{key: 3}, sample())
	must.MapEmpty(t, sample())

	// a2 is garbage collected: no negative increase; a new allocation with
	// restarts counts them all.
	index++
	must.NoError(t, st.DeleteEval(index, nil, []string{a2.ID}, false))
	a3 := mock.Alloc()
	a3.JobID, a3.Job = a1.JobID, a1.Job
	index++
	must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, index, []*structs.Allocation{a3}))
	update(withRestarts(a3, 1))
	must.Eq(t, map[metricsstore.GroupKey]int{key: 1}, sample())
	must.MapLen(t, 2, r.last)
}

// TestMetricsStore_NodeStateGauges checks the nodes of every datacenter and
// pool that are down, disconnected, draining and ineligible.
func TestMetricsStore_NodeStateGauges(t *testing.T) {
	ci.Parallel(t)
	st := state.TestStateStore(t)

	node := func(dc, status, eligibility string, drain bool) *structs.Node {
		n := mock.Node()
		n.Datacenter, n.Status, n.SchedulingEligibility = dc, status, eligibility
		if drain {
			n.DrainStrategy = &structs.DrainStrategy{DrainSpec: structs.DrainSpec{Deadline: time.Hour}}
		}
		return n
	}
	for i, n := range []*structs.Node{
		node("dc1", structs.NodeStatusReady, structs.NodeSchedulingEligible, false),
		node("dc1", structs.NodeStatusReady, structs.NodeSchedulingIneligible, true),
		node("dc1", structs.NodeStatusReady, structs.NodeSchedulingIneligible, false),
		node("dc1", structs.NodeStatusDown, structs.NodeSchedulingEligible, false),
		node("dc1", structs.NodeStatusDisconnected, structs.NodeSchedulingEligible, false),
		node("dc2", structs.NodeStatusReady, structs.NodeSchedulingEligible, false),
	} {
		must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, uint64(1000+i), n))
	}

	snap, err := st.Snapshot()
	must.NoError(t, err)
	gauges, err := nodeStateGauges(snap)
	must.NoError(t, err)
	must.Eq(t, map[string]float64{
		metricsstore.MetricNodesDown + "/dc1/default":         1,
		metricsstore.MetricNodesDisconnected + "/dc1/default": 1,
		metricsstore.MetricNodesDraining + "/dc1/default":     1,
		metricsstore.MetricNodesIneligible + "/dc1/default":   2,
		metricsstore.MetricNodesDown + "/dc2/default":         0,
		metricsstore.MetricNodesDisconnected + "/dc2/default": 0,
		metricsstore.MetricNodesDraining + "/dc2/default":     0,
		metricsstore.MetricNodesIneligible + "/dc2/default":   0,
	}, gaugeValues(gauges))
}

// TestMetricsStore_RecordNodeHost checks that the usage of a node is stored
// with the datacenter, pool and class of the node in the state, and that the
// usage of a node the state does not know is left out.
func TestMetricsStore_RecordNodeHost(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	node := mock.Node()
	node.NodePool = "gpu"
	must.NoError(t, s.fsm.State().UpsertNodePools(structs.MsgTypeTestSetup, 999, []*structs.NodePool{{Name: "gpu"}}))
	must.NoError(t, s.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	host := &structs.NodeHostUsage{Samples: 1, CPUPercent: structs.TaskGroupUsageValue{Avg: 10, Max: 10}}
	s.recordNodeHost(&structs.TaskGroupUsageReport{NodeID: node.ID, Host: host}, time.Now())
	s.recordNodeHost(&structs.TaskGroupUsageReport{NodeID: "unknown", Host: host}, time.Now())

	var series []metricsstore.GaugeSeries
	for _, g := range s.metricsStore.Gauges() {
		if g.Metric == metricsstore.MetricNodeCPUUsed {
			series = append(series, g)
		}
	}
	must.Eq(t, []metricsstore.GaugeSeries{{Metric: metricsstore.MetricNodeCPUUsed, Labels: map[string]string{
		"node": node.ID, "dc": "dc1", "pool": "gpu", "class": node.NodeClass,
	}}}, series)
}

// TestMetricsStore_WorkloadACL checks the ACL of the node and nodes series:
// node read, as the cluster series.
func TestMetricsStore_WorkloadACL(t *testing.T) {
	ci.Parallel(t)

	aclFor := func(rules string) *acl.ACL {
		p, err := acl.Parse(rules)
		must.NoError(t, err)
		a, err := acl.NewACL(false, []*acl.Policy{p})
		must.NoError(t, err)
		return a
	}
	nodes := aclFor(`node { policy = "read" }`)
	operator := aclFor(`operator { policy = "read" }`)
	jobs := aclFor(`namespace "default" { policy = "read" }`)
	nodeLabels := map[string]string{"node": "n1", "dc": "dc1", "pool": "default", "class": ""}
	for _, metric := range []string{metricsstore.MetricNodeCPUUsed, metricsstore.MetricNodeDiskUsedMB, metricsstore.MetricNodesDown} {
		must.True(t, allowGauge(nodes, metric, nodeLabels), must.Sprint(metric))
		must.False(t, allowGauge(operator, metric, nodeLabels), must.Sprint(metric))
		must.False(t, allowGauge(jobs, metric, nodeLabels), must.Sprint(metric))
	}
	// The node groups stay with the object ACL.
	must.False(t, allowGauge(nodes, metricsstore.MetricNodeGroupReady, map[string]string{"group": "g"}))
}
