// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
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
