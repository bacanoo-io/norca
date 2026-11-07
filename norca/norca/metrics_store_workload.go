// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sync"

	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The workload state (family 6) in the metrics store: the task restarts of
// the task groups and the nodes by state. The node host statistics (family
// 7) come with the usage reports of the clients (recordNodeHost).

// restartSampler turns the task restarts of the allocations in the state
// into their increase per task group since the previous sample.
type restartSampler struct {
	mu      sync.Mutex
	started bool
	// last are the restarts of the allocations that had any at the last
	// sample, by allocation ID.
	last map[string]uint64
}

// increase returns the task restarts of every task group since the previous
// call. The first call after the start of the server only takes the
// restarts so far: they happened before. An allocation the servers removed
// (garbage collected) no longer counts; it never makes an increase
// negative, and a count that went back counts nothing.
func (r *restartSampler) increase(snap *state.StateSnapshot) (map[metricsstore.GroupKey]int, error) {
	iter, err := snap.Allocs(nil, state.SortDefault)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]uint64, len(r.last))
	out := map[metricsstore.GroupKey]int{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		alloc := raw.(*structs.Allocation)
		var n uint64
		for _, ts := range alloc.TaskStates {
			if ts != nil {
				n += ts.Restarts
			}
		}
		if n == 0 {
			continue
		}
		seen[alloc.ID] = n
		if prev := r.last[alloc.ID]; r.started && n > prev {
			key := metricsstore.GroupKey{Namespace: alloc.Namespace, Job: alloc.JobID, Group: alloc.TaskGroup}
			out[key] += int(n - prev)
		}
	}
	r.last, r.started = seen, true
	return out, nil
}

// nodeStateGauges returns the nodes of every datacenter and node pool that
// are down, disconnected, ready and draining, and ready but ineligible for
// scheduling; zero for a datacenter and pool whose nodes are all fine.
func nodeStateGauges(snap *state.StateSnapshot) ([]metricsstore.Gauge, error) {
	iter, err := snap.Nodes(nil)
	if err != nil {
		return nil, err
	}
	type counts struct{ down, disconnected, draining, ineligible int }
	byPool := map[[2]string]*counts{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		node := raw.(*structs.Node)
		key := [2]string{node.Datacenter, node.NodePool}
		c := byPool[key]
		if c == nil {
			c = &counts{}
			byPool[key] = c
		}
		switch node.Status {
		case structs.NodeStatusDown:
			c.down++
		case structs.NodeStatusDisconnected:
			c.disconnected++
		case structs.NodeStatusReady:
			if node.DrainStrategy != nil {
				c.draining++
			}
			if node.SchedulingEligibility != structs.NodeSchedulingEligible {
				c.ineligible++
			}
		}
	}
	gauges := make([]metricsstore.Gauge, 0, 4*len(byPool))
	for key, c := range byPool {
		labels := map[string]string{"dc": key[0], "pool": key[1]}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricNodesDown, Labels: labels, Value: float64(c.down)},
			metricsstore.Gauge{Metric: metricsstore.MetricNodesDisconnected, Labels: labels, Value: float64(c.disconnected)},
			metricsstore.Gauge{Metric: metricsstore.MetricNodesDraining, Labels: labels, Value: float64(c.draining)},
			metricsstore.Gauge{Metric: metricsstore.MetricNodesIneligible, Labels: labels, Value: float64(c.ineligible)})
	}
	return gauges, nil
}
