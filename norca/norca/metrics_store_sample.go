// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"time"

	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// metricsStoreSampleLoop samples the allocations and the task restarts of the
// task groups, the capacity and the nodes by state of the datacenters and
// node pools, the health of the servers, the
// quotas, the scheduler and the control plane, the internals of the server
// (Raft, RPC and the system bus), and the custom objects into
// the metrics store, every interval. Every server has the state and the live
// usage of the nodes, so each samples its own store; what only the leader
// has, only the leader samples.
func (s *Server) metricsStoreSampleLoop(interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			return
		case now := <-tick.C:
			groups, err := s.allocCounts()
			if err != nil {
				s.logger.Warn("metrics store: reading the job summaries failed", "error", err)
				continue
			}
			s.metricsStore.RecordAllocs(now, groups)
			gauges, err := s.sampleGauges(now)
			if err != nil {
				s.logger.Warn("metrics store: sampling the cluster failed", "error", err)
				continue
			}
			s.metricsStore.RecordGauges(now, gauges)
		}
	}
}

// capacity sums the resources of the ready nodes of a datacenter and node
// pool.
type capacity struct {
	cpu, memory, disk                float64 // after the reserved resources
	allocCPU, allocMemory, allocDisk float64
	usedCPU, usedMemory              float64
	ready                            int
}

// add adds the sums of another node (or group of nodes) to c.
func (c *capacity) add(o *capacity) {
	c.cpu, c.memory, c.disk = c.cpu+o.cpu, c.memory+o.memory, c.disk+o.disk
	c.allocCPU, c.allocMemory, c.allocDisk = c.allocCPU+o.allocCPU, c.allocMemory+o.allocMemory, c.allocDisk+o.allocDisk
	c.usedCPU, c.usedMemory = c.usedCPU+o.usedCPU, c.usedMemory+o.usedMemory
	c.ready += o.ready
}

// sampleGauges returns the cluster, nodes by state, server health, quota,
// control plane, server internals and object gauges.
func (s *Server) sampleGauges(now time.Time) ([]metricsstore.Gauge, error) {
	snap, err := s.fsm.State().Snapshot()
	if err != nil {
		return nil, err
	}
	live := s.metricsStore.Live(now, 2*time.Minute)

	var gauges []metricsstore.Gauge
	cluster, err := s.sampleCapacity(snap, live)
	if err != nil {
		return nil, err
	}
	percent := func(part, whole float64) float64 {
		if whole <= 0 {
			return 0
		}
		return part / whole * 100
	}
	for key, c := range cluster {
		labels := map[string]string{"dc": key[0], "pool": key[1], "class": key[2]}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricClusterCPUAllocated, Labels: labels, Value: percent(c.allocCPU, c.cpu)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterCPUUsed, Labels: labels, Value: percent(c.usedCPU, c.cpu)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterMemoryAllocated, Labels: labels, Value: percent(c.allocMemory, c.memory)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterMemoryUsed, Labels: labels, Value: percent(c.usedMemory, c.memory)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterDiskAllocated, Labels: labels, Value: percent(c.allocDisk, c.disk)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterNodesReady, Labels: labels, Value: float64(c.ready)},
		)
	}

	gauges = append(gauges, emptyGroupGauges(snap, cluster)...)
	nodes, err := nodeStateGauges(snap)
	if err != nil {
		return nil, err
	}
	gauges = append(gauges, nodes...)

	// Only the leader runs autopilot and has the blocked evaluations.
	if s.IsLeader() {
		gauges = append(gauges, blockedGauges(snap, s.blockedEvals.Evals(), cluster)...)
		if h := s.GetClusterHealth(); h != nil {
			healthy := 0.0
			if h.Healthy {
				healthy = 1
			}
			gauges = append(gauges,
				metricsstore.Gauge{Metric: metricsstore.MetricAutopilotHealthy, Value: healthy},
				metricsstore.Gauge{Metric: metricsstore.MetricAutopilotFailureTolerance, Value: float64(h.FailureTolerance)})
		}
		voters, ready, promotions, demotions := s.standbyGauges()
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricAutopilotVoters, Value: float64(voters)},
			metricsstore.Gauge{Metric: metricsstore.MetricAutopilotStandbysReady, Value: float64(ready)},
			metricsstore.Gauge{Metric: metricsstore.MetricAutopilotStandbyPromotions, Value: float64(promotions)},
			metricsstore.Gauge{Metric: metricsstore.MetricAutopilotStandbyDemotions, Value: float64(demotions)})
	}

	quotas, err := s.sampleQuotas(snap, live)
	if err != nil {
		return nil, err
	}
	gauges = append(gauges, quotas...)

	// The scheduler and the control plane: the leader's own values (see
	// controlPlaneValues), the counters and the metrics store of this server,
	// and the jobs and the backups of the state.
	gauges = append(gauges, controlPlaneGauges(s.controlPlaneValues(), &s.metricsCounterLast)...)
	// The internals of this server: its Raft log store and replication, its
	// RPC requests, and its system bus.
	gauges = append(gauges, serverGauges(s.serverValues(), &s.metricsCounterLast)...)
	jobs, err := jobStatusGauges(snap)
	if err != nil {
		return nil, err
	}
	backups, err := backupGauges(snap, now)
	if err != nil {
		return nil, err
	}
	// The custom objects of the state.
	objects, err := objectGauges(snap)
	if err != nil {
		return nil, err
	}
	certs, err := nodeGroupCertGauges(snap, now)
	if err != nil {
		return nil, err
	}
	gauges = append(gauges, jobs...)
	gauges = append(gauges, backups...)
	gauges = append(gauges, objects...)
	return append(gauges, certs...), nil
}

// sampleCapacity sums the capacity, the allocated and the used resources of
// the ready, eligible nodes by datacenter, node pool and node class. The key
// {dc, pool, ""} is every node of the datacenter and pool; a node with a node
// class is also in {dc, pool, class}.
func (s *Server) sampleCapacity(snap *state.StateSnapshot, live *metricsstore.LiveUsage) (map[[3]string]*capacity, error) {
	iter, err := snap.Nodes(nil)
	if err != nil {
		return nil, err
	}
	out := map[[3]string]*capacity{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		node := raw.(*structs.Node)
		if node.Status != structs.NodeStatusReady || node.SchedulingEligibility != structs.NodeSchedulingEligible {
			continue
		}
		c := &capacity{ready: 1} // this node alone
		if total := node.ComparableResources(); total != nil {
			reserved := node.ComparableReservedResources()
			if reserved == nil {
				reserved = &structs.ComparableResources{}
			}
			c.cpu += float64(total.Flattened.Cpu.CpuShares - reserved.Flattened.Cpu.CpuShares)
			c.memory += float64(total.Flattened.Memory.MemoryMB - reserved.Flattened.Memory.MemoryMB)
			c.disk += float64(total.Shared.DiskMB - reserved.Shared.DiskMB)
		}
		allocs, err := snap.AllocsByNode(nil, node.ID)
		if err != nil {
			return nil, err
		}
		for _, alloc := range allocs {
			if alloc.ClientTerminalStatus() || alloc.ServerTerminalStatus() {
				continue
			}
			if r := alloc.ComparableResources(); r != nil {
				c.allocCPU += float64(r.Flattened.Cpu.CpuShares)
				c.allocMemory += float64(r.Flattened.Memory.MemoryMB)
				c.allocDisk += float64(r.Shared.DiskMB)
			}
		}
		if u, ok := live.Nodes[node.ID]; ok {
			c.usedCPU += u.CPUMHz
			c.usedMemory += u.MemoryMB
		}

		keys := [][3]string{{node.Datacenter, node.NodePool, ""}}
		if node.NodeClass != "" {
			keys = append(keys, [3]string{node.Datacenter, node.NodePool, node.NodeClass})
		}
		for _, key := range keys {
			sum := out[key]
			if sum == nil {
				sum = &capacity{}
				out[key] = sum
			}
			sum.add(c)
		}
	}
	return out, nil
}

// sampleQuotas returns the allocated and the used resources of the
// namespaces of every quota in the region.
func (s *Server) sampleQuotas(snap *state.StateSnapshot, live *metricsstore.LiveUsage) ([]metricsstore.Gauge, error) {
	used := map[string]metricsstore.Usage{}
	nsIter, err := snap.Namespaces(nil)
	if err != nil {
		return nil, err
	}
	for raw := nsIter.Next(); raw != nil; raw = nsIter.Next() {
		ns := raw.(*structs.Namespace)
		if ns.Quota == "" {
			continue
		}
		u := used[ns.Quota]
		l := live.Namespaces[ns.Name]
		u.CPUMHz += l.CPUMHz
		u.MemoryMB += l.MemoryMB
		used[ns.Quota] = u
	}

	iter, err := snap.QuotaUsages(nil, state.SortDefault)
	if err != nil {
		return nil, err
	}
	var gauges []metricsstore.Gauge
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		usage := raw.(*structs.QuotaUsage)
		var cpu, memory float64
		for _, limit := range usage.Used {
			if limit == nil || limit.RegionLimit == nil {
				continue
			}
			cpu += float64(limit.RegionLimit.CPU)
			memory += float64(limit.RegionLimit.MemoryMB)
		}
		labels := map[string]string{"quota": usage.Name}
		u := used[usage.Name]
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricQuotaCPUAllocated, Labels: labels, Value: cpu},
			metricsstore.Gauge{Metric: metricsstore.MetricQuotaMemoryAllocated, Labels: labels, Value: memory},
			metricsstore.Gauge{Metric: metricsstore.MetricQuotaCPUUsed, Labels: labels, Value: u.CPUMHz},
			metricsstore.Gauge{Metric: metricsstore.MetricQuotaMemoryUsed, Labels: labels, Value: u.MemoryMB})
	}
	return gauges, nil
}

// emptyGroupGauges returns the gauges of the node groups whose datacenter,
// pool and class have no ready node: nodes_ready = 0, and nothing allocated.
// Without them a group that starts from nothing (a class that no node has) or
// whose nodes are all down has no series, its ready nodes can not be told
// from a gauge that can not be read, and its headroom is not known.
func emptyGroupGauges(snap *state.StateSnapshot, cluster map[[3]string]*capacity) []metricsstore.Gauge {
	iter, err := snap.CustomObjects(nil, structs.NodeScalingGroup, structs.NodeGroupKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return nil
	}
	seen := map[[3]string]bool{}
	var gauges []metricsstore.Gauge
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		if obj.Deleting() {
			continue
		}
		spec := obj.Spec
		str := func(k string) string { v, _ := spec[k].(string); return v }
		key := [3]string{str("datacenter"), str("pool"), str("node_class")}
		if cluster[key] != nil || seen[key] || key[0] == "" || key[1] == "" {
			continue
		}
		seen[key] = true
		labels := map[string]string{"dc": key[0], "pool": key[1], "class": key[2]}
		for _, metric := range []string{
			metricsstore.MetricClusterNodesReady,
			metricsstore.MetricClusterCPUAllocated,
			metricsstore.MetricClusterMemoryAllocated,
		} {
			gauges = append(gauges, metricsstore.Gauge{Metric: metric, Labels: labels, Value: 0})
		}
	}
	return gauges
}
