// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"time"

	"github.com/hashicorp/nomad/nomad/metricsstore"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// metricsStoreSampleLoop samples the allocations of the task groups, the
// capacity of the datacenters and node pools, the health of the servers and
// the quotas into the metrics store, every interval. Every server has the
// state and the live usage of the nodes, so each samples its own store.
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

// sampleGauges returns the cluster, server health and quota gauges.
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
		labels := map[string]string{"dc": key[0], "pool": key[1]}
		gauges = append(gauges,
			metricsstore.Gauge{Metric: metricsstore.MetricClusterCPUAllocated, Labels: labels, Value: percent(c.allocCPU, c.cpu)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterCPUUsed, Labels: labels, Value: percent(c.usedCPU, c.cpu)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterMemoryAllocated, Labels: labels, Value: percent(c.allocMemory, c.memory)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterMemoryUsed, Labels: labels, Value: percent(c.usedMemory, c.memory)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterDiskAllocated, Labels: labels, Value: percent(c.allocDisk, c.disk)},
			metricsstore.Gauge{Metric: metricsstore.MetricClusterNodesReady, Labels: labels, Value: float64(c.ready)},
		)
	}

	// Only the leader runs autopilot.
	if s.IsLeader() {
		if h := s.GetClusterHealth(); h != nil {
			healthy := 0.0
			if h.Healthy {
				healthy = 1
			}
			gauges = append(gauges,
				metricsstore.Gauge{Metric: metricsstore.MetricAutopilotHealthy, Value: healthy},
				metricsstore.Gauge{Metric: metricsstore.MetricAutopilotFailureTolerance, Value: float64(h.FailureTolerance)})
		}
	}

	quotas, err := s.sampleQuotas(snap, live)
	if err != nil {
		return nil, err
	}
	return append(gauges, quotas...), nil
}

// sampleCapacity sums the capacity, the allocated and the used resources of
// the ready, eligible nodes by datacenter and node pool.
func (s *Server) sampleCapacity(snap *state.StateSnapshot, live *metricsstore.LiveUsage) (map[[2]string]*capacity, error) {
	iter, err := snap.Nodes(nil)
	if err != nil {
		return nil, err
	}
	out := map[[2]string]*capacity{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		node := raw.(*structs.Node)
		if node.Status != structs.NodeStatusReady || node.SchedulingEligibility != structs.NodeSchedulingEligible {
			continue
		}
		key := [2]string{node.Datacenter, node.NodePool}
		c := out[key]
		if c == nil {
			c = &capacity{}
			out[key] = c
		}
		c.ready++
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
