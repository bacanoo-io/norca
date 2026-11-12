// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestPreferByJob(t *testing.T) {
	ci.Parallel(t)

	a := &demandGroup{name: "workers-a", dc: "dc-a"}
	b := &demandGroup{name: "workers-b", dc: "dc-b"}
	c := &demandGroup{name: "workers-c", dc: "dc-c"}
	item := func(u Unplaced) *demandItem { return &demandItem{Unplaced: u} }

	tests := []struct {
		name     string
		item     *demandItem
		groups   []*demandGroup
		added    map[string]int
		expected []*demandGroup
	}{
		{
			name:     "a job that says nothing leaves the choice to the expander",
			item:     item(Unplaced{}),
			groups:   []*demandGroup{a, b},
			expected: []*demandGroup{a, b},
		},
		{
			name: "an even spread prefers the datacenter with the fewest allocations",
			item: item(Unplaced{Spread: &DatacenterSpread{
				Total: 10, Placed: map[string]int{"dc-a": 1, "dc-b": 5},
			}}),
			groups:   []*demandGroup{a, b},
			expected: []*demandGroup{a},
		},
		{
			name: "what was given in this look counts",
			item: item(Unplaced{Spread: &DatacenterSpread{
				Total: 10, Placed: map[string]int{"dc-a": 3, "dc-b": 4},
			}}),
			groups:   []*demandGroup{a, b},
			added:    map[string]int{"dc-a": 1},
			expected: []*demandGroup{a, b},
		},
		{
			name: "targets give the shares",
			item: item(Unplaced{Spread: &DatacenterSpread{
				Total: 10, Targets: map[string]int{"dc-a": 20, "dc-b": 80},
				Placed: map[string]int{"dc-a": 2, "dc-b": 4},
			}}),
			groups:   []*demandGroup{a, b},
			expected: []*demandGroup{b},
		},
		{
			name: "a datacenter without a group still has its share",
			item: item(Unplaced{Spread: &DatacenterSpread{
				Total: 9, Placed: map[string]int{"dc-a": 3, "dc-b": 1, "dc-c": 1},
			}}),
			groups:   []*demandGroup{a, b},
			expected: []*demandGroup{b},
		},
		{
			name: "every datacenter at its share: the expander decides",
			item: item(Unplaced{Spread: &DatacenterSpread{
				Total: 4, Placed: map[string]int{"dc-a": 2, "dc-b": 2},
			}}),
			groups:   []*demandGroup{a, b},
			expected: []*demandGroup{a, b},
		},
		{
			name:     "an affinity prefers its datacenter",
			item:     item(Unplaced{PreferredDatacenters: []string{"dc-b"}}),
			groups:   []*demandGroup{a, b, c},
			expected: []*demandGroup{b},
		},
		{
			name:     "an affinity for a datacenter without a group changes nothing",
			item:     item(Unplaced{PreferredDatacenters: []string{"dc-x"}}),
			groups:   []*demandGroup{a, b},
			expected: []*demandGroup{a, b},
		},
		{
			name: "a spread comes before an affinity",
			item: item(Unplaced{
				Spread:               &DatacenterSpread{Total: 4, Placed: map[string]int{"dc-a": 0, "dc-b": 2}},
				PreferredDatacenters: []string{"dc-b"},
			}),
			groups:   []*demandGroup{a, b},
			expected: []*demandGroup{a},
		},
		{
			name: "one group has no choice to make",
			item: item(Unplaced{Spread: &DatacenterSpread{
				Total: 10, Placed: map[string]int{"dc-a": 9},
			}}),
			groups:   []*demandGroup{a},
			expected: []*demandGroup{a},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added := tt.added
			if added == nil {
				added = map[string]int{}
			}
			must.Eq(t, groupNames(tt.expected), groupNames(preferByJob(tt.item, tt.groups, added)))
		})
	}
}

func groupNames(groups []*demandGroup) []string {
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.name)
	}
	return names
}

// The nodes for a job that spreads its allocations over the datacenters are
// added where the job is short, not where the expander of the pool would
// add them.
func TestDemand_FollowsTheSpreadOfTheJob(t *testing.T) {
	ci.Parallel(t)

	// Ten are wanted, five in each datacenter; dc-a has one and dc-b five.
	// The four that are blocked need a node each.
	spread := &DatacenterSpread{Total: 10, Placed: map[string]int{"dc-a": 1, "dc-b": 5}}
	work := func(s *DatacenterSpread) map[[2]string][]Unplaced {
		out := map[[2]string][]Unplaced{}
		for _, dc := range []string{"dc-a", "dc-b"} {
			out[[2]string{dc, "workers"}] = []Unplaced{{
				Namespace: "default", Job: "web", Group: "g", Count: 4, CPU: 100, Memory: 6000, Spread: s,
			}}
		}
		return out
	}
	// workers-a has five nodes and workers-b one: the expander of the pool
	// (spread: the fewest nodes) would grow workers-b.
	groups := []testGroup{
		{name: "workers-a", dc: "dc-a", spec: map[string]any{"replicas": float64(5)}, status: map[string]any{"replicas": float64(5)}},
		{name: "workers-b", dc: "dc-b"},
	}

	tests := []struct {
		name     string
		spread   *DatacenterSpread
		expected map[string]int64
	}{
		{name: "without a spread the expander chooses", spread: nil, expected: map[string]int64{"workers-b": 5}},
		{name: "with a spread the job is filled where it is short", spread: spread, expected: map[string]int64{"workers-a": 9}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, objects, _ := demandHarness(t, groups, work(tt.spread))
			must.NoError(t, h.runner.scaleForDemand(context.Background()))
			must.Eq(t, tt.expected, scaledTo(objects))
		})
	}
}
