// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func topologyIDs(regs []*ServiceRegistration) []string {
	ids := make([]string, len(regs))
	for i, reg := range regs {
		ids[i] = reg.ID
	}
	return ids
}

func TestSelectByTopology(t *testing.T) {
	ci.Parallel(t)

	inst := func(id, dc, zone string) *ServiceRegistration {
		return &ServiceRegistration{ID: id, Datacenter: dc, Zone: zone}
	}
	near := Topology{Region: "eu", Datacenter: "dc1", Zone: "a"}

	all := []*ServiceRegistration{
		inst("a1", "dc1", "a"), inst("b1", "dc1", "b"), inst("a2", "dc1", "a"),
		inst("c1", "dc2", "c"), inst("b2", "dc1", "b"), inst("c2", "dc2", "c"),
		inst("c3", "dc2", "c"), inst("c4", "dc2", "c"), inst("c5", "dc2", "c"), inst("c6", "dc2", "c"),
	}

	// no settings, none and region return everything as it is
	must.Eq(t, topologyIDs(all), topologyIDs(SelectByTopology(all, near, nil)))
	must.Eq(t, topologyIDs(all), topologyIDs(SelectByTopology(all, near, &ServiceDiscovery{Prefer: "none", MinLocal: 5})))
	must.Eq(t, topologyIDs(all), topologyIDs(SelectByTopology(all, near, &ServiceDiscovery{Prefer: "region"})))

	zone := &ServiceDiscovery{Prefer: DiscoveryPreferZone, MinLocal: 2, OverflowFraction: 0.2}

	// two healthy instances in the zone of the consumer: only those
	must.Eq(t, []string{"a1", "a2"}, topologyIDs(SelectByTopology(all, near, zone)))

	// one left: the rest of the datacenter joins
	one := []*ServiceRegistration{all[0], all[1], all[3], all[4], all[5]}
	must.Eq(t, []string{"a1", "b1", "b2"}, topologyIDs(SelectByTopology(one, near, zone)))

	// none in the zone nor the datacenter: the rest of the region
	remote := []*ServiceRegistration{all[3], all[5]}
	must.Eq(t, []string{"c1", "c2"}, topologyIDs(SelectByTopology(remote, near, zone)))

	// the overflow fraction pulls in the next tier when the zone holds too
	// small a share of all instances
	big := &ServiceDiscovery{Prefer: DiscoveryPreferZone, MinLocal: 2, OverflowFraction: 0.5}
	must.Eq(t, []string{"a1", "a2", "b1", "b2", "c1", "c2", "c3", "c4", "c5", "c6"},
		topologyIDs(SelectByTopology(all, near, big)))
	big.OverflowFraction = 0.4
	must.Eq(t, []string{"a1", "a2", "b1", "b2"}, topologyIDs(SelectByTopology(all, near, big)))

	// a consumer without a zone starts at its datacenter
	must.Eq(t, []string{"a1", "b1", "a2", "b2"},
		topologyIDs(SelectByTopology(all, Topology{Region: "eu", Datacenter: "dc1"}, zone)))

	// datacenter preference
	dc := &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter, MinLocal: 1}
	must.Eq(t, []string{"c1", "c2", "c3", "c4", "c5", "c6"},
		topologyIDs(SelectByTopology(all, Topology{Region: "eu", Datacenter: "dc2"}, dc)))
}
