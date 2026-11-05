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
