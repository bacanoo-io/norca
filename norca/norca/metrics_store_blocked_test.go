// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// TestMetricsStore_BlockedGauges checks the demand of the blocked
// evaluations: the unplaced allocations (the failed one and those coalesced
// into it) times the resources of the task group, in the datacenters with
// nodes, with the distinct_hosts allocations apart, and without an
// evaluation that waits for a quota.
func TestMetricsStore_BlockedGauges(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	st := s.fsm.State()

	node := mock.Node()
	node.NodePool = "default"
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	blocked := func(index uint64, mutate func(*structs.Job)) *structs.Evaluation {
		job := mock.Job()
		job.NodePool = "default"
		job.TaskGroups[0].Tasks[0].Resources.CPU = 500
		job.TaskGroups[0].Tasks[0].Resources.MemoryMB = 256
		if mutate != nil {
			mutate(job)
		}
		must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))
		eval := mock.BlockedEval()
		eval.JobID = job.ID
		eval.Namespace = job.Namespace
		eval.FailedTGAllocs = map[string]*structs.AllocMetric{
			job.TaskGroups[0].Name: {
				NodesAvailable:    map[string]int{node.Datacenter: 1},
				CoalescedFailures: 2, // three unplaced allocations
			},
		}
		s.blockedEvals.Block(eval)
		return eval
	}
	blocked(1001, nil)
	blocked(1002, func(j *structs.Job) {
		j.Constraints = append(j.Constraints, &structs.Constraint{Operand: structs.ConstraintDistinctHosts})
	})
	quota := blocked(1003, nil)
	quota.QuotaLimitReached = "q"

	gauges, err := s.sampleGauges(time.Now())
	must.NoError(t, err)
	values := map[string]float64{}
	for _, g := range gauges {
		if g.Labels["dc"] == node.Datacenter && g.Labels["pool"] == "default" {
			values[g.Metric] = g.Value
		}
	}

	// The quota evaluation was blocked after the pointer was taken; it is
	// the same object, so it is skipped.
	must.Eq(t, 2*3*500.0, values[metricsstore.MetricClusterBlockedCPU])
	must.Eq(t, 2*3*256.0, values[metricsstore.MetricClusterBlockedMemory])
	must.Eq(t, 2.0, values[metricsstore.MetricClusterBlockedEvals])
	must.Eq(t, 3.0, values[metricsstore.MetricClusterBlockedAllocsExclusive])
}

// TestMetricsStore_BlockedDistinctProperty checks that the allocations of the
// job count against a distinct_property by the datacenter of their node, and
// that a constraint on another property is reported as unverifiable.
func TestMetricsStore_BlockedDistinctProperty(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	st := s.fsm.State()

	node := mock.Node()
	node.NodePool = "default"
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	blocked := func(index uint64, target string) {
		job := mock.Job()
		job.NodePool = "default"
		job.TaskGroups[0].Constraints = append(job.TaskGroups[0].Constraints,
			&structs.Constraint{Operand: structs.ConstraintDistinctProperty, LTarget: target, RTarget: "2"})
		must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))
		alloc := mock.Alloc()
		alloc.Job, alloc.JobID, alloc.Namespace = job, job.ID, job.Namespace
		alloc.TaskGroup = job.TaskGroups[0].Name
		alloc.NodeID = node.ID
		must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{alloc}))
		eval := mock.BlockedEval()
		eval.JobID, eval.Namespace = job.ID, job.Namespace
		eval.FailedTGAllocs = map[string]*structs.AllocMetric{
			job.TaskGroups[0].Name: {NodesAvailable: map[string]int{node.Datacenter: 1}},
		}
		s.blockedEvals.Block(eval)
	}
	blocked(1001, "${node.datacenter}")
	blocked(1010, "${meta.rack}")

	snap, err := st.Snapshot()
	must.NoError(t, err)
	d := collectBlocked(snap, s.blockedEvals.Evals())[[2]string{node.Datacenter, "default"}]
	must.NotNil(t, d)
	must.Len(t, 2, d.unplaced)
	for _, u := range d.unplaced {
		if u.Unverifiable != "" {
			continue
		}
		must.Len(t, 1, u.Distinct)
		must.Eq(t, 2, u.Distinct[0].Limit)
		must.Eq(t, 1, u.Distinct[0].Used[node.Datacenter])
		got, ok := u.Restrict(autoscaler.NodeGroupKey{Pool: "default", Datacenter: node.Datacenter})
		must.True(t, ok)
		must.Eq(t, 1, got.Count) // one more fits under the limit of two
	}

	gauges := blockedGauges(snap, s.blockedEvals.Evals(), nil)
	var unverifiable float64
	for _, g := range gauges {
		if g.Metric == metricsstore.MetricClusterBlockedUnverifiable {
			unverifiable += g.Value
		}
	}
	must.Eq(t, 1.0, unverifiable)
}

// TestMetricsStore_BlockedStaticPort checks that a task group with a port by
// number needs a node for each of its allocations, however small they are,
// and that a dynamic port does not.
func TestMetricsStore_BlockedStaticPort(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	st := s.fsm.State()

	node := mock.Node()
	node.NodePool = "default"
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	blocked := func(index uint64, ports ...structs.Port) *structs.Job {
		job := mock.Job()
		job.NodePool = "default"
		job.TaskGroups[0].Networks = []*structs.NetworkResource{{Mode: "host", ReservedPorts: ports}}
		job.TaskGroups[0].Tasks[0].Resources.CPU = 50
		job.TaskGroups[0].Tasks[0].Resources.MemoryMB = 32
		must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))
		eval := mock.BlockedEval()
		eval.JobID, eval.Namespace = job.ID, job.Namespace
		eval.FailedTGAllocs = map[string]*structs.AllocMetric{
			job.TaskGroups[0].Name: {NodesAvailable: map[string]int{node.Datacenter: 1}, CoalescedFailures: 2},
		}
		s.blockedEvals.Block(eval)
		return job
	}
	proxy := blocked(1001, structs.Port{Label: "http", Value: 80}, structs.Port{Label: "https", Value: 443})
	dynamic := blocked(1002, structs.Port{Label: "dyn"})

	snap, err := st.Snapshot()
	must.NoError(t, err)
	d := collectBlocked(snap, s.blockedEvals.Evals())[[2]string{node.Datacenter, "default"}]
	must.NotNil(t, d)
	must.Len(t, 2, d.unplaced)
	for _, u := range d.unplaced {
		must.Eq(t, u.Job == proxy.ID, u.Exclusive, must.Sprintf("job %s (dynamic is %s)", u.Job, dynamic.ID))
	}
}

// TestMetricsStore_BlockedNoDatacenter checks the work of a job that names
// its datacenters by a pattern while no datacenter has a node for it: it is
// kept for node scaling under no datacenter, with the patterns of the job,
// and makes no gauge series.
func TestMetricsStore_BlockedNoDatacenter(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	st := s.fsm.State()

	job := mock.Job()
	job.NodePool = "default"
	job.Datacenters = []string{"*"}
	must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, 1001, nil, job))
	eval := mock.BlockedEval()
	eval.JobID = job.ID
	eval.Namespace = job.Namespace
	eval.FailedTGAllocs = map[string]*structs.AllocMetric{
		job.TaskGroups[0].Name: {NodesAvailable: map[string]int{}, CoalescedFailures: 1},
	}

	snap, err := st.Snapshot()
	must.NoError(t, err)
	demand := collectBlocked(snap, []*structs.Evaluation{eval})

	must.MapLen(t, 1, demand)
	unplaced := demand[[2]string{"", "default"}]
	must.NotNil(t, unplaced)
	must.Len(t, 1, unplaced.unplaced)
	must.Eq(t, 2, unplaced.unplaced[0].Count)
	must.Eq(t, []string{"*"}, unplaced.unplaced[0].Datacenters)

	for _, g := range blockedGauges(snap, []*structs.Evaluation{eval}, nil) {
		must.NotEq(t, "", g.Labels["dc"], must.Sprintf("the series %s has no datacenter", g.Metric))
	}
}

func TestDatacenterSpread(t *testing.T) {
	ci.Parallel(t)

	st := state.TestStateStore(t)
	nodeA, nodeB := mock.Node(), mock.Node()
	nodeA.Datacenter, nodeB.Datacenter = "dc-a", "dc-b"
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 1000, nodeA))
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 1001, nodeB))

	job := mock.Job()
	tg := job.TaskGroups[0]
	tg.Count = 10
	must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, 1002, nil, job))

	// Three live allocations in dc-a, one in dc-b, and a stopped one that
	// does not count.
	alloc := func(node *structs.Node, clientStatus string) *structs.Allocation {
		a := mock.Alloc()
		a.Job, a.JobID, a.Namespace, a.TaskGroup = job, job.ID, job.Namespace, tg.Name
		a.NodeID, a.ClientStatus = node.ID, clientStatus
		return a
	}
	must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, 1003, []*structs.Allocation{
		alloc(nodeA, structs.AllocClientStatusRunning),
		alloc(nodeA, structs.AllocClientStatusRunning),
		alloc(nodeA, structs.AllocClientStatusPending),
		alloc(nodeB, structs.AllocClientStatusRunning),
		alloc(nodeB, structs.AllocClientStatusComplete),
	}))
	snap, err := st.Snapshot()
	must.NoError(t, err)

	onDatacenter := func(targets ...*structs.SpreadTarget) *structs.Spread {
		return &structs.Spread{Attribute: "${node.datacenter}", Weight: 100, SpreadTarget: targets}
	}
	placed := map[string]int{"dc-a": 3, "dc-b": 1}

	tests := []struct {
		name         string
		jobSpreads   []*structs.Spread
		groupSpreads []*structs.Spread
		expected     *autoscaler.DatacenterSpread
	}{
		{
			name:     "no spread",
			expected: nil,
		},
		{
			name:       "a spread on another attribute",
			jobSpreads: []*structs.Spread{{Attribute: "${meta.rack}", Weight: 100}},
			expected:   nil,
		},
		{
			name:       "an even spread of the job",
			jobSpreads: []*structs.Spread{onDatacenter()},
			expected:   &autoscaler.DatacenterSpread{Total: 10, Targets: map[string]int{}, Placed: placed},
		},
		{
			name:       "the spread of the group comes before the job's",
			jobSpreads: []*structs.Spread{onDatacenter()},
			groupSpreads: []*structs.Spread{onDatacenter(
				&structs.SpreadTarget{Value: "dc-a", Percent: 20},
				&structs.SpreadTarget{Value: "dc-b", Percent: 80},
			)},
			expected: &autoscaler.DatacenterSpread{
				Total: 10, Targets: map[string]int{"dc-a": 20, "dc-b": 80}, Placed: placed,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := job.Copy()
			j.Spreads = tt.jobSpreads
			j.TaskGroups[0].Spreads = tt.groupSpreads
			must.Eq(t, tt.expected, datacenterSpread(snap, j, j.TaskGroups[0]))
		})
	}
}

func TestPreferredDatacenters(t *testing.T) {
	ci.Parallel(t)

	affinity := func(target, operand, value string, weight int8) *structs.Affinity {
		return &structs.Affinity{LTarget: target, Operand: operand, RTarget: value, Weight: weight}
	}

	tests := []struct {
		name            string
		jobAffinities   []*structs.Affinity
		groupAffinities []*structs.Affinity
		expected        []string
	}{
		{name: "no affinity", expected: nil},
		{
			name:          "an affinity for a datacenter",
			jobAffinities: []*structs.Affinity{affinity("${node.datacenter}", "=", "dc-a", 50)},
			expected:      []string{"dc-a"},
		},
		{
			name:            "of the job and of the group, each once",
			jobAffinities:   []*structs.Affinity{affinity("${node.datacenter}", "=", "dc-a", 50)},
			groupAffinities: []*structs.Affinity{affinity("${node.datacenter}", "is", "dc-b", 10), affinity("${node.datacenter}", "==", "dc-a", 10)},
			expected:        []string{"dc-a", "dc-b"},
		},
		{
			name:          "a datacenter the job avoids is not preferred",
			jobAffinities: []*structs.Affinity{affinity("${node.datacenter}", "=", "dc-a", -50)},
			expected:      nil,
		},
		{
			name:          "another attribute or operand says nothing of a datacenter",
			jobAffinities: []*structs.Affinity{affinity("${node.class}", "=", "gpu", 50), affinity("${node.datacenter}", "!=", "dc-a", 50)},
			expected:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := mock.Job()
			job.Affinities = tt.jobAffinities
			job.TaskGroups[0].Affinities = tt.groupAffinities
			must.Eq(t, tt.expected, preferredDatacenters(job, job.TaskGroups[0]))
		})
	}
}
