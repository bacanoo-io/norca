// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
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

	inst := func(id, dc string) *ServiceRegistration {
		return &ServiceRegistration{ID: id, Datacenter: dc}
	}
	near := Topology{Region: "eu", Datacenter: "a"}

	all := []*ServiceRegistration{
		inst("a1", "a"), inst("b1", "b"), inst("a2", "a"),
		inst("c1", "c"), inst("b2", "b"), inst("c2", "c"),
		inst("c3", "c"), inst("c4", "c"), inst("c5", "c"), inst("c6", "c"),
	}

	// no settings, none and region return everything as it is
	must.Eq(t, topologyIDs(all), topologyIDs(SelectByTopology(all, near, nil)))
	must.Eq(t, topologyIDs(all), topologyIDs(SelectByTopology(all, near, &ServiceDiscovery{Prefer: "none", MinLocal: 5})))
	must.Eq(t, topologyIDs(all), topologyIDs(SelectByTopology(all, near, &ServiceDiscovery{Prefer: "region"})))

	dc := &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter, MinLocal: 2, OverflowFraction: pointer.Of(0.2)}

	// two healthy instances in the datacenter of the consumer: only those
	must.Eq(t, []string{"a1", "a2"}, topologyIDs(SelectByTopology(all, near, dc)))

	// one left: the rest of the region joins, in its order
	one := []*ServiceRegistration{all[0], all[1], all[3], all[4], all[5]}
	must.Eq(t, []string{"a1", "b1", "c1", "b2", "c2"}, topologyIDs(SelectByTopology(one, near, dc)))

	// none in the datacenter: the rest of the region
	remote := []*ServiceRegistration{all[3], all[5]}
	must.Eq(t, []string{"c1", "c2"}, topologyIDs(SelectByTopology(remote, near, dc)))

	// the overflow fraction pulls in the next tier when the datacenter holds
	// too small a share of all instances
	big := &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter, MinLocal: 2, OverflowFraction: pointer.Of(0.5)}
	must.Eq(t, []string{"a1", "a2", "b1", "c1", "b2", "c2", "c3", "c4", "c5", "c6"},
		topologyIDs(SelectByTopology(all, near, big)))
	big.OverflowFraction = pointer.Of(0.2)
	must.Eq(t, []string{"a1", "a2"}, topologyIDs(SelectByTopology(all, near, big)))

	// min_local 1 in a large datacenter
	one1 := &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter, MinLocal: 1}
	must.Eq(t, []string{"c1", "c2", "c3", "c4", "c5", "c6"},
		topologyIDs(SelectByTopology(all, Topology{Region: "eu", Datacenter: "c"}, one1)))
}
