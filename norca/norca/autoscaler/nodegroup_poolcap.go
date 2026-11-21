// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"math"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// checkPoolCap returns the largest scale the checks of a node group may set
// within the limits of its pool: max_nodes for the groups of the pool together
// and max_step for one decision (review finding ASC-3). The blocked work and
// the headroom were held to them (demand.go, apply); the checks of the group
// went up to the group's own maximum. It returns math.MaxInt64 when the pool
// has no limit. It never returns less than the scale the group has: the
// limits hold a raise, not a scale down.
func checkPoolCap(store *state.StateStore, cfg *NodeScalingConfig, group *structs.CustomObject) int64 {
	ps := cfg.pool(asString(group.Spec["pool"]))
	if ps.MaxNodes <= 0 && ps.MaxStep <= 0 {
		return math.MaxInt64
	}
	spec := asInt(group.Spec["replicas"])
	capped := int64(math.MaxInt64)
	if ps.MaxStep > 0 {
		capped = spec + int64(ps.MaxStep)
	}
	if ps.MaxNodes > 0 {
		total, err := poolNodes(store, ps, asString(group.Spec["pool"]))
		if err != nil {
			return spec
		}
		capped = min(capped, spec+int64(ps.MaxNodes)-total)
	}
	return max(capped, spec)
}

// poolNodes counts the nodes of the node groups of a pool as max_nodes counts
// them (NodePoolScaling.counted): spec.replicas, but of a group that cannot
// get its nodes only those it has, unless the pool counts the spec.
func poolNodes(store *state.StateStore, ps NodePoolScaling, pool string) (int64, error) {
	iter, err := store.CustomObjects(nil, structs.NodeScalingGroup, structs.NodeGroupKind,
		structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return 0, err
	}
	var total int64
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		if obj.Deleting() || asString(obj.Spec["pool"]) != pool {
			continue
		}
		spec := asInt(obj.Spec["replicas"])
		if ps.MaxNodesCounts != MaxNodesCountsSpec && unableReason(obj.Status) != "" {
			spec = min(spec, asInt(obj.Status["replicas"])+asInt(obj.Status["joining"]))
		}
		total += spec
	}
	return total, nil
}
