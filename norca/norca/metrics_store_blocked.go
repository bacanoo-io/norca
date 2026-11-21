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
//
// The scheduler blocks an evaluation for any placement that failed, whatever
// the reason. Work that every node was filtered out for by a check a new node
// of a group would fail too (a constraint on an attribute or meta, a driver,
// a device, a volume) is reported as unverifiable, so it does not grow a group
// (review finding ASC-1); and the node groups whose nodes the scheduler found
// ineligible for the work are named, so it does not grow them.
func collectBlocked(snap *state.StateSnapshot, evals []*structs.Evaluation) map[[2]string]*blockedDemand {
	demand := map[[2]string]*blockedDemand{}
	groupClasses := nodeGroupClasses(snap)
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
				JobIndex:  job.JobModifyIndex,
				Group:     name,
				EvalID:    eval.ID,
				Count:     1 + metric.CoalescedFailures,
				Exclusive: hasConstraint(job.Constraints, structs.ConstraintDistinctHosts) ||
					hasConstraint(tg.Constraints, structs.ConstraintDistinctHosts) ||
					hasStaticPort(tg),
			}
			u.Datacenters = job.Datacenters
			u.Spread = datacenterSpread(snap, job, tg)
			u.PreferredDatacenters = preferredDatacenters(job, tg)
			u.Distinct, u.Unverifiable = distinctProperties(snap, job, tg)
			u.Classes = classConstraints(job, tg, &u)
			if u.Unverifiable == "" {
				u.Unverifiable = filteredForNewNodes(job, tg, metric)
			}
			u.IneligibleGroups = ineligibleNodeGroups(eval, groupClasses)
			for _, task := range tg.Tasks {
				if task.Resources != nil {
					u.CPU += float64(task.Resources.CPU)
					u.Memory += float64(task.Resources.MemoryMB)
				}
			}
			if tg.EphemeralDisk != nil {
				u.Disk = float64(tg.EphemeralDisk.SizeMB)
			}
			// Work that no datacenter with nodes could take, of a job that
			// names its datacenters by patterns only, has no datacenter to
			// be reported in. It is kept under no datacenter for node
			// scaling, which finds the node groups by the patterns.
			dcs := blockedDatacenters(job, metric)
			if len(dcs) == 0 {
				dcs = []string{""}
			}
			for _, dc := range dcs {
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
		// The demand under no datacenter has no series to go to.
		if k[0] == "" {
			continue
		}
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

// datacenterAttribute is the node attribute of the datacenter in spreads and
// affinities.
const datacenterAttribute = "${node.datacenter}"

// datacenterSpread returns the spread of a task group over the datacenters:
// its own spread on ${node.datacenter}, or else the job's; nil without one.
// The live allocations of the group are counted by the datacenter of their
// node.
func datacenterSpread(snap *state.StateSnapshot, job *structs.Job, tg *structs.TaskGroup) *autoscaler.DatacenterSpread {
	var spread *structs.Spread
	for _, candidates := range [][]*structs.Spread{tg.Spreads, job.Spreads} {
		for _, s := range candidates {
			if s != nil && s.Attribute == datacenterAttribute {
				spread = s
				break
			}
		}
		if spread != nil {
			break
		}
	}
	if spread == nil {
		return nil
	}

	out := &autoscaler.DatacenterSpread{Total: tg.Count, Targets: map[string]int{}, Placed: map[string]int{}}
	for _, target := range spread.SpreadTarget {
		if target != nil {
			out.Targets[target.Value] = int(target.Percent)
		}
	}
	allocs, _ := snap.AllocsByJob(nil, job.Namespace, job.ID, false)
	for _, alloc := range allocs {
		if alloc.TerminalStatus() || alloc.TaskGroup != tg.Name {
			continue
		}
		if node, _ := snap.NodeByID(nil, alloc.NodeID); node != nil {
			out.Placed[node.Datacenter]++
		}
	}
	return out
}

// preferredDatacenters returns the datacenters the job or the task group has
// an affinity for: a positive weight on ${node.datacenter} being equal to a
// name.
func preferredDatacenters(job *structs.Job, tg *structs.TaskGroup) []string {
	var out []string
	for _, affinities := range [][]*structs.Affinity{job.Affinities, tg.Affinities} {
		for _, a := range affinities {
			isEqual := a != nil && (a.Operand == "=" || a.Operand == "==" || a.Operand == "is")
			if isEqual && a.LTarget == datacenterAttribute && a.Weight > 0 && !slices.Contains(out, a.RTarget) {
				out = append(out, a.RTarget)
			}
		}
	}
	return out
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

// computedClassIneligible is the reason the scheduler records for a node it
// skips because an earlier node of the same computed class failed a check:
// that check is recorded too, on the earlier node.
const computedClassIneligible = "computed class ineligible"

// filteredForNewNodes returns the reason a node that a node group adds would
// not take the work either, or "" when a new node could take it (review
// finding ASC-1).
//
// A new node can take the work when the datacenter had no node to try, or
// when a node passed every check and only its free resources were short
// (NodesExhausted). When every node was filtered out, each reason must be one
// a new node of a group passes: distinct_hosts (it runs nothing of the job
// yet), a distinct_property (counted by the demand), a constraint on
// ${node.class} that the demand checks against the class of each group, and
// the computed class of a node skipped after another of its class failed. Any
// other reason (an attribute or meta, a driver, a device, a host volume, a CSI
// volume or plugin, a network, a colocate or separate rule, a quota) would
// stop a new node the same way.
func filteredForNewNodes(job *structs.Job, tg *structs.TaskGroup, metric *structs.AllocMetric) string {
	if metric.NodesEvaluated == 0 || metric.NodesExhausted > 0 {
		return ""
	}
	classes := map[string]bool{}
	for _, cs := range [][]*structs.Constraint{job.Constraints, tg.Constraints} {
		for _, c := range cs {
			if c.LTarget == "${node.class}" {
				classes[c.String()] = true
			}
		}
	}
	for _, task := range tg.Tasks {
		for _, c := range task.Constraints {
			if c.LTarget == "${node.class}" {
				classes[c.String()] = true
			}
		}
	}
	reasons := make([]string, 0, len(metric.ConstraintFiltered))
	for reason := range metric.ConstraintFiltered {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	for _, reason := range reasons {
		switch {
		case reason == structs.ConstraintDistinctHosts,
			reason == computedClassIneligible,
			strings.HasPrefix(reason, structs.ConstraintDistinctProperty+":"),
			classes[reason]:
			continue
		}
		return "every node was filtered out for a reason a new node would have too: " + reason
	}
	return ""
}

// nodeGroupClasses returns the computed classes of the nodes of each node
// group, by the name the nodes carry in their meta.
func nodeGroupClasses(snap *state.StateSnapshot) map[string][]string {
	iter, err := snap.Nodes(nil)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		node := raw.(*structs.Node)
		group := node.Meta[structs.NodeMetaNodeGroup]
		if group == "" || node.ComputedClass == "" || slices.Contains(out[group], node.ComputedClass) {
			continue
		}
		out[group] = append(out[group], node.ComputedClass)
	}
	return out
}

// ineligibleNodeGroups returns the node groups every node of which is of a
// computed class the scheduler found ineligible for the evaluation: a new
// node of such a group is of the same class, and the evaluation would not
// even be unblocked by it (review finding ASC-1). An evaluation that escaped
// the class tracking (a constraint on something a class does not stand for)
// names none.
func ineligibleNodeGroups(eval *structs.Evaluation, groupClasses map[string][]string) []string {
	if eval.EscapedComputedClass || len(eval.ClassEligibility) == 0 {
		return nil
	}
	var out []string
	for group, classes := range groupClasses {
		ineligible := len(classes) > 0
		for _, class := range classes {
			if eligible, known := eval.ClassEligibility[class]; !known || eligible {
				ineligible = false
				break
			}
		}
		if ineligible {
			out = append(out, group)
		}
	}
	slices.Sort(out)
	return out
}
