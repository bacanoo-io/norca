// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// blockedDemand is the demand of the blocked evaluations in one datacenter
// and node pool.
type blockedDemand struct {
	evals    int                   // blocked evaluations
	unplaced []autoscaler.Unplaced // by task group
}

// collectBlocked returns the demand of the blocked evaluations by datacenter
// and node pool.
//
// An evaluation counts when it waits for capacity: one blocked by the quota
// of its namespace would not be helped by more nodes, so it does not. The
// unplaced allocations of a task group are the one that failed and those
// coalesced into it. The demand of a job that can use several datacenters is
// counted in each of them.
func collectBlocked(snap *state.StateSnapshot, evals []*structs.Evaluation) map[[2]string]*blockedDemand {
	demand := map[[2]string]*blockedDemand{}
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
		counted := map[[2]string]bool{} // where this evaluation is counted
		for name, metric := range eval.FailedTGAllocs {
			tg := job.LookupTaskGroup(name)
			if tg == nil || metric == nil {
				continue
			}
			u := autoscaler.Unplaced{
				Namespace: job.Namespace,
				Job:       job.ID,
				Group:     name,
				EvalID:    eval.ID,
				Count:     1 + metric.CoalescedFailures,
				Exclusive: hasConstraint(job.Constraints, structs.ConstraintDistinctHosts) ||
					hasConstraint(tg.Constraints, structs.ConstraintDistinctHosts) ||
					hasStaticPort(tg),
			}
			u.Distinct, u.Unverifiable = distinctProperties(snap, job, tg)
			u.Classes = classConstraints(job, tg, &u)
			for _, task := range tg.Tasks {
				if task.Resources != nil {
					u.CPU += float64(task.Resources.CPU)
					u.Memory += float64(task.Resources.MemoryMB)
				}
			}
			if tg.EphemeralDisk != nil {
				u.Disk = float64(tg.EphemeralDisk.SizeMB)
			}
			for _, dc := range blockedDatacenters(job, metric) {
				k := [2]string{dc, pool}
				d := demand[k]
				if d == nil {
					d = &blockedDemand{}
					demand[k] = d
				}
				d.unplaced = append(d.unplaced, u)
				if !counted[k] {
					counted[k] = true
					d.evals++
				}
			}
		}
	}
	return demand
}

// blockedGauges returns the blocked demand gauges per datacenter and node
// pool. Every datacenter and pool of the capacity gets a value, zero when
// nothing is blocked, so the series do not end when the demand does.
func blockedGauges(snap *state.StateSnapshot, evals []*structs.Evaluation, cluster map[[3]string]*capacity) []metricsstore.Gauge {
	demand := collectBlocked(snap, evals)
	for key := range cluster {
		k := [2]string{key[0], key[1]}
		if demand[k] == nil {
			demand[k] = &blockedDemand{}
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
		var cpu, memory float64
		exclusive, unverifiable := 0, 0
		for _, u := range d.unplaced {
			if u.Unverifiable != "" {
				unverifiable += u.Count
			}
			cpu += u.CPU * float64(u.Count)
			memory += u.Memory * float64(u.Count)
			if u.Exclusive {
				exclusive += u.Count
			}
		}
		labels := map[string]string{"dc": k[0], "pool": k[1]}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedCPU, Labels: labels, Value: cpu},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedMemory, Labels: labels, Value: memory},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedEvals, Labels: labels, Value: float64(d.evals)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedAllocsExclusive, Labels: labels, Value: float64(exclusive)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterBlockedUnverifiable, Labels: labels, Value: float64(unverifiable)})
	}
	return gauges
}

// distinctProperties returns the distinct_property constraints of a task
// group (its own and those of the job) with the live allocations that count
// against them, by the value of the property. A constraint on a property
// other than the datacenter or the node pool is returned as unverifiable:
// what a node that does not exist yet would have is not known.
func distinctProperties(snap *state.StateSnapshot, job *structs.Job, tg *structs.TaskGroup) (out []autoscaler.DistinctProperty, unverifiable string) {
	type source struct {
		c        *structs.Constraint
		jobLevel bool
	}
	var sources []source
	for _, c := range job.Constraints {
		if c.Operand == structs.ConstraintDistinctProperty {
			sources = append(sources, source{c, true})
		}
	}
	for _, c := range tg.Constraints {
		if c.Operand == structs.ConstraintDistinctProperty {
			sources = append(sources, source{c, false})
		}
	}
	if len(sources) == 0 {
		return nil, ""
	}

	allocs, _ := snap.AllocsByJob(nil, job.Namespace, job.ID, false)
	for _, src := range sources {
		target := src.c.LTarget
		if target != "${node.datacenter}" && target != "${node.pool}" {
			return nil, src.c.String()
		}
		limit := 1
		if n, err := strconv.Atoi(src.c.RTarget); err == nil && n > 0 {
			limit = n
		}
		used := map[string]int{}
		for _, alloc := range allocs {
			if alloc.TerminalStatus() || (!src.jobLevel && alloc.TaskGroup != tg.Name) {
				continue
			}
			node, _ := snap.NodeByID(nil, alloc.NodeID)
			if node == nil {
				continue
			}
			if target == "${node.datacenter}" {
				used[node.Datacenter]++
			} else {
				used[node.NodePool]++
			}
		}
		out = append(out, autoscaler.DistinctProperty{Target: target, Limit: limit, Used: used})
	}
	return out, ""
}

// blockedDatacenters are the datacenters the blocked demand of a task group
// could be placed in: those with nodes the scheduler looked at and the
// datacenters the job names without a wildcard (one without nodes yet, in a
// group scaled down to zero, is not among the first).
func blockedDatacenters(job *structs.Job, metric *structs.AllocMetric) []string {
	var dcs []string
	for dc := range metric.NodesAvailable {
		dcs = append(dcs, dc)
	}
	for _, dc := range job.Datacenters {
		if dc != "" && !strings.Contains(dc, "*") && !slices.Contains(dcs, dc) {
			dcs = append(dcs, dc)
		}
	}
	slices.Sort(dcs)
	return dcs
}

// hasStaticPort reports whether the task group asks for a port by number on
// the host: two of its allocations cannot share a node, whatever their size,
// and a node that has the port taken by other work cannot take it either. A
// proxy on ports 80 and 443 is blocked this way.
func hasStaticPort(tg *structs.TaskGroup) bool {
	static := func(networks structs.Networks) bool {
		for _, n := range networks {
			if n == nil {
				continue
			}
			for _, p := range n.ReservedPorts {
				if p.Value > 0 {
					return true
				}
			}
		}
		return false
	}
	if static(tg.Networks) {
		return true
	}
	for _, task := range tg.Tasks {
		if task.Resources != nil && static(task.Resources.Networks) {
			return true
		}
	}
	return false
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

// classConstraints returns the constraints of a task group on the class of
// the node (its own, those of the job and of its tasks). One with an operand
// that cannot be checked against a group makes the demand unverifiable.
func classConstraints(job *structs.Job, tg *structs.TaskGroup, u *autoscaler.Unplaced) []autoscaler.ClassConstraint {
	all := append([]*structs.Constraint{}, job.Constraints...)
	all = append(all, tg.Constraints...)
	for _, task := range tg.Tasks {
		all = append(all, task.Constraints...)
	}
	var out []autoscaler.ClassConstraint
	for _, c := range all {
		if c.LTarget != "${node.class}" {
			continue
		}
		if !autoscaler.ClassConstraintSupported(c.Operand, c.RTarget) {
			if u.Unverifiable == "" {
				u.Unverifiable = c.String()
			}
			continue
		}
		out = append(out, autoscaler.ClassConstraint{Operand: c.Operand, Value: c.RTarget})
	}
	return out
}
