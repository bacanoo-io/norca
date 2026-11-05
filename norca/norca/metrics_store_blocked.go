// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"cmp"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// blockedDemand is the demand of the blocked evaluations in one datacenter
// and node pool.
type blockedDemand struct {
	cpu, memory float64
	evals       int
	exclusive   int // unplaced allocations that need a node of their own
}

// blockedGauges returns the blocked demand gauges per datacenter and node
// pool. Every datacenter and pool of the capacity gets a value, zero when
// nothing is blocked, so the series do not end when the demand does.
//
// An evaluation counts when it waits for capacity: one blocked by the quota
// of its namespace would not be helped by more nodes, so it does not. The
// unplaced allocations of a task group are the one that failed and those
// coalesced into it.
func blockedGauges(snap *state.StateSnapshot, evals []*structs.Evaluation, cluster map[[3]string]*capacity) []metricsstore.Gauge {
	demand := map[[2]string]*blockedDemand{}
	for key := range cluster {
		demand[[2]string{key[0], key[1]}] = &blockedDemand{}
	}
	get := func(dc, pool string) *blockedDemand {
		k := [2]string{dc, pool}
		d := demand[k]
		if d == nil {
			d = &blockedDemand{}
			demand[k] = d
		}
		return d
	}

	for _, eval := range evals {
		if eval.QuotaLimitReached != "" {
			continue
		}
		job, err := snap.JobByID(nil, eval.Namespace, eval.JobID)
		if err != nil || job == nil || job.Stopped() {
			continue
		}
		pool := job.NodePool
		if pool == "" {
			pool = structs.NodePoolDefault
		}
		counted := map[string]bool{} // the datacenters this evaluation is counted in
		for name, metric := range eval.FailedTGAllocs {
			tg := job.LookupTaskGroup(name)
			if tg == nil || metric == nil {
				continue
			}
			unplaced := 1 + metric.CoalescedFailures
			var cpu, memory float64
			for _, task := range tg.Tasks {
				if task.Resources != nil {
					cpu += float64(task.Resources.CPU)
					memory += float64(task.Resources.MemoryMB)
				}
			}
			exclusive := hasConstraint(job.Constraints, structs.ConstraintDistinctHosts) ||
				hasConstraint(tg.Constraints, structs.ConstraintDistinctHosts)
			for _, dc := range blockedDatacenters(job, metric) {
				d := get(dc, pool)
				d.cpu += cpu * float64(unplaced)
				d.memory += memory * float64(unplaced)
				if exclusive {
					d.exclusive += unplaced
				}
				if !counted[dc] {
					counted[dc] = true
					d.evals++
				}
			}
		}
	}

	keys := make([][2]string, 0, len(demand))
	for k := range demand {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	})
	var gauges []metricsstore.Gauge
	for _, k := range keys {
		d := demand[k]
		labels := map[string]string{"dc": k[0], "pool": k[1]}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedCPU, Labels: labels, Value: d.cpu},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedMemory, Labels: labels, Value: d.memory},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedEvals, Labels: labels, Value: float64(d.evals)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedAllocsExclusive, Labels: labels, Value: float64(d.exclusive)})
	}
	return gauges
}

// blockedDatacenters are the datacenters the blocked demand of a task group
// could be placed in: those with nodes the scheduler looked at, and, when it
// found none (a group scaled down to zero nodes), the datacenters the job
// names without a wildcard.
func blockedDatacenters(job *structs.Job, metric *structs.AllocMetric) []string {
	var dcs []string
	for dc := range metric.NodesAvailable {
		dcs = append(dcs, dc)
	}
	if len(dcs) == 0 {
		for _, dc := range job.Datacenters {
			if dc != "" && !strings.Contains(dc, "*") {
				dcs = append(dcs, dc)
			}
		}
	}
	slices.Sort(dcs)
	return dcs
}

// hasConstraint reports whether one of the constraints has the operand.
func hasConstraint(constraints []*structs.Constraint, operand string) bool {
	for _, c := range constraints {
		if c.Operand == operand {
			return true
		}
	}
	return false
}
