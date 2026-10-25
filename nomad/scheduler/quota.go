// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// quotaStateReader is the part of the state store used for quota
// enforcement. It is kept separate from the State interface so that states
// without quota support (for example in tests) simply don't enforce quotas.
type quotaStateReader interface {
	QuotaForNamespace(ws memdb.WatchSet, namespace string) (*structs.QuotaSpec, error)
	QuotaUsageByName(ws memdb.WatchSet, name string) (*structs.QuotaUsage, error)
	NamespaceByName(ws memdb.WatchSet, name string) (*structs.Namespace, error)
}

// QuotaIterator is a FeasibleIterator that filters out placements that would
// make the quota of the job's namespace exceed its limit in this region. The
// usage it checks against is the usage in the scheduler's state snapshot plus
// the effect of the plan built so far, so placements, stops and in-place
// updates of the current evaluation are all taken into account. The final,
// authoritative check is done by the plan applier.
//
// When a placement is rejected, the quota is recorded with
// EvalEligibility.SetQuotaLimitReached so the resulting blocked evaluation is
// unblocked when quota is freed.
type QuotaIterator struct {
	ctx    Context
	source FeasibleIterator

	// Set per job
	quota    string
	limit    *structs.QuotaLimit
	used     structs.QuotaResources
	inQuota  map[string]bool
	existing map[string]structs.QuotaResources

	// Set per task group selection
	tg        *structs.TaskGroup
	usesCores bool
	planUsed  structs.QuotaResources
	exhausted []string
	reported  map[string]struct{}
}

// NewQuotaIterator returns a quota iterator wrapping the source iterator.
func NewQuotaIterator(ctx Context, source FeasibleIterator) FeasibleIterator {
	return &QuotaIterator{
		ctx:    ctx,
		source: source,
	}
}

// SetJob looks up the quota limit that applies to the job's namespace.
func (iter *QuotaIterator) SetJob(job *structs.Job) {
	iter.quota = ""
	iter.limit = nil
	iter.used = structs.QuotaResources{}
	iter.inQuota = nil
	iter.existing = nil
	iter.tg = nil

	reader, ok := iter.ctx.State().(quotaStateReader)
	if !ok || job == nil {
		return
	}

	spec, err := reader.QuotaForNamespace(nil, job.Namespace)
	if err != nil {
		iter.ctx.Logger().Error("failed to look up namespace quota", "namespace", job.Namespace, "error", err)
		return
	}
	if spec == nil {
		return
	}

	region := iter.ctx.State().Config().Region
	limit := spec.LimitForRegion(region)
	if limit == nil {
		return
	}

	usage, err := reader.QuotaUsageByName(nil, spec.Name)
	if err != nil {
		iter.ctx.Logger().Error("failed to look up quota usage", "quota", spec.Name, "error", err)
		return
	}
	if entry := usage.RegionUsage(region); entry != nil {
		iter.used = entry.UsageResources()
	}

	iter.quota = spec.Name
	iter.limit = limit
	iter.inQuota = map[string]bool{job.Namespace: true}
	iter.existing = make(map[string]structs.QuotaResources)
}

// SetTaskGroup computes the usage including the current plan and, unless the
// task group reserves cores (whose MHz depend on the node), whether one more
// allocation of the group fits into the quota.
func (iter *QuotaIterator) SetTaskGroup(tg *structs.TaskGroup) {
	iter.tg = tg
	iter.exhausted = nil
	iter.reported = nil
	if iter.limit == nil {
		return
	}

	delta, err := structs.QuotaPlanDelta(iter.ctx.Plan(), iter.namespaceInQuota, iter.existingUsage)
	if err != nil {
		iter.ctx.Logger().Error("failed to compute quota usage of plan", "quota", iter.quota, "error", err)
		delta = structs.QuotaResources{}
	}
	iter.planUsed = iter.used.Add(delta)

	iter.usesCores = structs.TaskGroupUsesCores(tg)
	if !iter.usesCores {
		iter.exhausted = iter.limit.CheckAdditional(iter.planUsed, structs.TaskGroupQuotaResources(tg, 0))
	}
}

func (iter *QuotaIterator) namespaceInQuota(namespace string) (bool, error) {
	if in, ok := iter.inQuota[namespace]; ok {
		return in, nil
	}
	reader, ok := iter.ctx.State().(quotaStateReader)
	if !ok {
		return false, nil
	}
	ns, err := reader.NamespaceByName(nil, namespace)
	if err != nil {
		return false, err
	}
	in := ns != nil && ns.Quota == iter.quota
	iter.inQuota[namespace] = in
	return in, nil
}

func (iter *QuotaIterator) existingUsage(allocID string) (structs.QuotaResources, error) {
	if r, ok := iter.existing[allocID]; ok {
		return r, nil
	}
	alloc, err := iter.ctx.State().AllocByID(nil, allocID)
	if err != nil {
		return structs.QuotaResources{}, err
	}
	r := structs.AllocQuotaResources(alloc)
	iter.existing[allocID] = r
	return r, nil
}

// Next returns the next node that doesn't violate the quota.
func (iter *QuotaIterator) Next() *structs.Node {
	if iter.limit == nil || iter.tg == nil {
		return iter.source.Next()
	}

	if len(iter.exhausted) > 0 {
		// The demand doesn't depend on the node: no node is feasible.
		iter.markExhausted(iter.exhausted)
		return nil
	}

	for {
		option := iter.source.Next()
		if option == nil || !iter.usesCores {
			return option
		}

		demand := structs.TaskGroupQuotaResources(iter.tg, sharesPerCore(option))
		dims := iter.limit.CheckAdditional(iter.planUsed, demand)
		if len(dims) == 0 {
			return option
		}
		iter.ctx.Metrics().FilterNode(option, "quota exhausted")
		iter.markExhausted(dims)
	}
}

// markExhausted records the exhausted dimensions in the allocation metrics
// and marks the evaluation as blocked on the quota.
func (iter *QuotaIterator) markExhausted(dims []string) {
	if iter.reported == nil {
		iter.reported = make(map[string]struct{}, len(dims))
	}
	var fresh []string
	for _, dim := range dims {
		if _, ok := iter.reported[dim]; ok {
			continue
		}
		iter.reported[dim] = struct{}{}
		fresh = append(fresh, dim)
	}
	if len(fresh) > 0 {
		iter.ctx.Metrics().ExhaustQuota(fresh)
	}
	iter.ctx.Eligibility().SetQuotaLimitReached(iter.quota)
}

func (iter *QuotaIterator) Reset() {
	iter.source.Reset()
}

// sharesPerCore returns the MHz of one core of the node, or zero if unknown.
func sharesPerCore(node *structs.Node) int64 {
	if node == nil || node.NodeResources == nil || node.NodeResources.Cpu.TotalCpuCores == 0 {
		return 0
	}
	return node.NodeResources.Cpu.SharesPerCore()
}
