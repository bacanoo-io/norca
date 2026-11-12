// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import "fmt"

// The reserve for the loss of a datacenter. The headroom of a node group is
// measured against its own datacenter, so the capacity that replaces a lost
// datacenter is bought after the loss, and the work waits for machines. A
// pool with reserve_datacenter_loss keeps it ahead of time: every group
// keeps free the share it would get of the work of the other datacenter of
// the pool that carries the most, so the pool without any one of its
// datacenters still holds the allocated work. For n even datacenters that is
// 1/(n-1) more capacity.
//
// The reserve of a group is free capacity like its headroom, and the larger
// of the two applies: it raises the scale ahead of the work (after the
// cooldown of the group) and keeps the scale down from taking it.
//
// No reserve is kept while a group of the pool is not healthy: a datacenter
// is lost then, the reserve is what its work is placed on, and buying
// another one for the datacenters that are left is not what was asked for.
// Nor when the use of a group can't be read, or the pool is in one
// datacenter.

// aheadWanted returns the free capacity the group keeps ahead of the work, in
// nodes: its headroom (a number of nodes, or a share of the nodes it has) or
// its reserve for the loss of a datacenter, whichever is the most.
func (g *demandGroup) aheadWanted(nodes float64) float64 {
	return max(float64(g.headNodes), g.headPercent/100*nodes, g.reserve)
}

// keepsAhead reports whether the group keeps any capacity free ahead of the
// work.
func (g *demandGroup) keepsAhead() bool {
	return g.headNodes > 0 || g.headPercent > 0 || g.reserve > 0
}

// aheadReason says what the free capacity of the group is short of.
func (g *demandGroup) aheadReason(wanted float64, pending int64) string {
	what := "headroom"
	if g.reserve > 0 && wanted == g.reserve {
		what = "reserve for the loss of datacenter " + g.reserveFor
	}
	return fmt.Sprintf("%s: %.1f nodes free (and %d on their way), %.1f wanted", what, g.free, pending, wanted)
}

// setReserves sets the reserve of the groups of the pools that keep one.
func (d *demandRun) setReserves() {
	byPool := map[string][]*demandGroup{}
	for _, g := range d.groups {
		if d.cfg.pool(g.pool).ReserveDatacenterLoss {
			byPool[g.pool] = append(byPool[g.pool], g)
		}
	}
	for _, groups := range byPool {
		setPoolReserve(groups)
	}
}

// setPoolReserve sets the reserve of the groups of one pool: for each group
// the largest share it would get of the work of another datacenter, in its
// own nodes. The work of a datacenter that is lost is split between the
// groups of the others by their weights.
func setPoolReserve(groups []*demandGroup) {
	// With the sizes of the nodes the work is counted in CPU and memory, so
	// groups of different nodes can stand in for each other; without them in
	// nodes.
	isSized := true
	for _, g := range groups {
		if g.unable != "" || g.degraded != "" || !g.freeKnown {
			return
		}
		isSized = isSized && g.sizeKnown
	}

	type load struct{ cpu, memory float64 }
	used := map[string]load{} // by datacenter
	var dcs []string
	for _, g := range groups {
		nodes := max(g.freeNodes-g.free, 0)
		l, seen := used[g.dc]
		if !seen {
			dcs = append(dcs, g.dc)
		}
		if isSized {
			l.cpu, l.memory = l.cpu+nodes*g.size.CPU, l.memory+nodes*g.size.Memory
		} else {
			l.cpu, l.memory = l.cpu+nodes, l.memory+nodes
		}
		used[g.dc] = l
	}
	if len(dcs) < 2 {
		return
	}

	for _, g := range groups {
		for _, lost := range dcs {
			if lost == g.dc {
				continue
			}
			var weights int64
			for _, other := range groups {
				if other.dc != lost {
					weights += other.weight
				}
			}
			l := used[lost]
			nodes := l.cpu
			if isSized {
				nodes = max(l.cpu/g.size.CPU, l.memory/g.size.Memory)
			}
			if share := nodes * float64(g.weight) / float64(weights); share > g.reserve {
				g.reserve, g.reserveFor = share, lost
			}
		}
	}
}
