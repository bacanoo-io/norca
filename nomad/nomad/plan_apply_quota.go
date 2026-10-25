// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// evaluatePlanQuota returns whether the plan would be over quota. This is the
// authoritative quota check: the snapshot includes the results of all plans
// applied before, so parallel schedulers can't jointly exceed a quota. A plan
// is only rejected if it increases the usage of a dimension beyond its
// limit; plans that keep or reduce the usage of an over-committed quota are
// allowed.
func evaluatePlanQuota(snap *state.StateSnapshot, plan *structs.Plan) (bool, error) {
	dims, err := planQuotaViolations(snap, plan)
	return len(dims) > 0, err
}

// planQuotaViolations returns the quota dimensions the plan would exceed.
func planQuotaViolations(snap *state.StateSnapshot, plan *structs.Plan) ([]string, error) {
	if plan == nil || plan.Job == nil {
		return nil, nil
	}

	spec, err := snap.QuotaForNamespace(nil, plan.Job.Namespace)
	if err != nil || spec == nil {
		return nil, err
	}
	region := snap.Config().Region
	limit := spec.LimitForRegion(region)
	if limit == nil {
		return nil, nil
	}

	usage, err := snap.QuotaUsageByName(nil, spec.Name)
	if err != nil {
		return nil, err
	}
	var used structs.QuotaResources
	if entry := usage.RegionUsage(region); entry != nil {
		used = entry.UsageResources()
	}

	inQuota := func(namespace string) (bool, error) {
		ns, err := snap.NamespaceByName(nil, namespace)
		if err != nil || ns == nil {
			return false, err
		}
		return ns.Quota == spec.Name, nil
	}
	existing := func(allocID string) (structs.QuotaResources, error) {
		alloc, err := snap.AllocByID(nil, allocID)
		if err != nil {
			return structs.QuotaResources{}, err
		}
		return structs.AllocQuotaResources(alloc), nil
	}

	delta, err := structs.QuotaPlanDelta(plan, inQuota, existing)
	if err != nil {
		return nil, err
	}
	return limit.CheckAdditional(used, delta), nil
}
