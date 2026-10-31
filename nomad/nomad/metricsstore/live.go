// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// liveMergeWindow is how close in time the parts of a split usage report
// are: parts published within it add up, a later report replaces them.
const liveMergeWindow = 2 * time.Second

// Usage is an amount of CPU (MHz) and memory (MiB).
type Usage struct {
	CPUMHz   float64
	MemoryMB float64
}

// nodeLive is the latest usage report of a node: the usage of all
// allocations of each namespace.
type nodeLive struct {
	at         time.Time
	namespaces map[string]Usage
	total      Usage
	interval   time.Duration
}

// recordLiveLocked keeps the usage of a report as the current usage of its
// node; s.mu is held.
func (s *Store) recordLiveLocked(report *structs.TaskGroupUsageReport, at time.Time) {
	if report.NodeID == "" {
		return
	}
	n := s.live[report.NodeID]
	if n == nil || at.Sub(n.at) > liveMergeWindow || at.Before(n.at) {
		if n != nil && at.Before(n.at) {
			return // an older report read back after a restart
		}
		n = &nodeLive{at: at, namespaces: map[string]Usage{}, interval: report.Window}
		s.live[report.NodeID] = n
	}
	for _, g := range report.Groups {
		if g == nil || g.Samples <= 0 {
			continue
		}
		ns := g.Namespace
		if ns == "" {
			ns = structs.DefaultNamespace
		}
		allocs := float64(g.Allocs)
		u := Usage{CPUMHz: g.CPUMHz.Avg * allocs, MemoryMB: g.MemoryMB.Avg * allocs}
		cur := n.namespaces[ns]
		cur.CPUMHz += u.CPUMHz
		cur.MemoryMB += u.MemoryMB
		n.namespaces[ns] = cur
		n.total.CPUMHz += u.CPUMHz
		n.total.MemoryMB += u.MemoryMB
	}
}

// LiveUsage is the current usage of the nodes whose latest report is not
// older than two report intervals (or maxAge without an interval).
type LiveUsage struct {
	// Nodes is the usage of all allocations of each node.
	Nodes map[string]Usage
	// Namespaces is the usage of all allocations of each namespace.
	Namespaces map[string]Usage
}

// Live returns the current usage of the nodes and the namespaces.
func (s *Store) Live(now time.Time, maxAge time.Duration) *LiveUsage {
	out := &LiveUsage{Nodes: map[string]Usage{}, Namespaces: map[string]Usage{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, n := range s.live {
		limit := maxAge
		if n.interval > 0 {
			limit = 2*n.interval + liveMergeWindow
		}
		if now.Sub(n.at) > limit {
			delete(s.live, id)
			continue
		}
		out.Nodes[id] = n.total
		for ns, u := range n.namespaces {
			cur := out.Namespaces[ns]
			cur.CPUMHz += u.CPUMHz
			cur.MemoryMB += u.MemoryMB
			out.Namespaces[ns] = cur
		}
	}
	return out
}
