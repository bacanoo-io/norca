// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

// Where a job wants its allocations among the datacenters. The blocked work
// trigger gives work to the node groups that can take it and lets the
// expander of the pool choose among them; the expander knows the groups, not
// the job. A job that spreads its allocations over the datacenters, or
// prefers one, has said where the new nodes should be: without it the
// scheduler places the allocations on whatever nodes were added, the spread
// being only a score, and the job loses the balance it asked for.

// DatacenterSpread is the spread of a task group over the datacenters
// (a spread on ${node.datacenter}), and where its allocations are.
type DatacenterSpread struct {
	// Total is the count of the task group.
	Total int

	// Targets are the shares of the datacenters the spread names, in
	// percent. Without targets the spread is even over the datacenters the
	// allocations may go to.
	Targets map[string]int

	// Placed counts the live allocations of the task group by datacenter.
	Placed map[string]int
}

// wanted returns how many allocations of the group the spread wants in a
// datacenter, of dcs datacenters the work may go to.
func (s *DatacenterSpread) wanted(dc string, dcs int) float64 {
	if len(s.Targets) > 0 {
		return float64(s.Total) * float64(s.Targets[dc]) / 100
	}
	if dcs == 0 {
		return 0
	}
	return float64(s.Total) / float64(dcs)
}

// preferByJob narrows the node groups that could take one allocation of the
// work to those the job wants it in: the datacenters furthest below the
// share of the job's spread, or else the datacenters the job has an affinity
// for. added counts the allocations of the work given to each datacenter in
// this look. When the job says nothing, or no group is where it wants, the
// groups are returned as they are and the expander of the pool decides.
func preferByJob(item *demandItem, groups []*demandGroup, added map[string]int) []*demandGroup {
	if len(groups) < 2 {
		return groups
	}
	if spread := item.Spread; spread != nil {
		dcs := map[string]bool{}
		for dc := range spread.Placed {
			dcs[dc] = true
		}
		for _, g := range groups {
			dcs[g.dc] = true
		}
		var preferred []*demandGroup
		largest := 0.0
		for _, g := range groups {
			missing := spread.wanted(g.dc, len(dcs)) - float64(spread.Placed[g.dc]+added[g.dc])
			switch {
			case missing <= 0 || missing < largest:
				continue
			case missing > largest:
				largest, preferred = missing, preferred[:0]
			}
			preferred = append(preferred, g)
		}
		if len(preferred) > 0 {
			return preferred
		}
		return groups
	}

	if len(item.PreferredDatacenters) > 0 {
		var preferred []*demandGroup
		for _, g := range groups {
			for _, dc := range item.PreferredDatacenters {
				if dc == g.dc {
					preferred = append(preferred, g)
					break
				}
			}
		}
		if len(preferred) > 0 {
			return preferred
		}
	}
	return groups
}
