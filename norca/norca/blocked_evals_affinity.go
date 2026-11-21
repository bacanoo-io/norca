// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// A required colocate rule (scheduler/alloc_affinity.go) filters out every
// node where no allocation the rule selects runs. An evaluation blocked by it
// waits for an allocation of another job, which is neither a change of a node
// nor of a quota, the two things that unblock evaluations: without more it
// stayed blocked until something unrelated changed a node (review finding
// SCH-4). A plan that places allocations in a namespace therefore unblocks
// the evaluations whose colocate rules select allocations of that namespace;
// one that still finds no node blocks again.

// allocAffinityColocatePrefix starts the reason the scheduler records for a
// node a required colocate rule filtered out: "colocate {ns/sel} by scope", or
// "colocate {sel} by scope" for a rule on the namespace of its job.
const allocAffinityColocatePrefix = structs.AllocAffinityKindColocate + " {"

// colocateTargets returns the namespaces of the required colocate rules that
// filtered out nodes for the evaluation, as its failed placements record them.
func colocateTargets(eval *structs.Evaluation) []string {
	var out []string
	for _, metric := range eval.FailedTGAllocs {
		if metric == nil {
			continue
		}
		for reason := range metric.ConstraintFiltered {
			if !strings.HasPrefix(reason, allocAffinityColocatePrefix) {
				continue
			}
			sel := strings.TrimPrefix(reason, allocAffinityColocatePrefix)
			if end := strings.Index(sel, "}"); end >= 0 {
				sel = sel[:end]
			}
			ns := eval.Namespace
			if slash := strings.Index(sel, "/"); slash >= 0 && !strings.Contains(sel[:slash], "=") {
				ns = sel[:slash]
			}
			found := false
			for _, have := range out {
				found = found || have == ns
			}
			if !found {
				out = append(out, ns)
			}
		}
	}
	return out
}

// UnblockAllocAffinity unblocks the evaluations that wait for a required
// colocate rule on one of the namespaces a plan placed allocations in, at the
// index of the plan.
func (b *BlockedEvals) UnblockAllocAffinity(namespaces map[string]struct{}, index uint64) {
	if len(namespaces) == 0 {
		return
	}
	b.l.Lock()
	defer b.l.Unlock()
	if !b.enabled {
		return
	}
	for ns := range namespaces {
		b.affinityIndexes[ns] = index
	}

	unblocked := make(map[*structs.Evaluation]string)
	escapedUnblocked, numQuotaLimit := 0, 0
	for _, escaped := range []bool{false, true} {
		set := b.captured
		if escaped {
			set = b.escaped
		}
		for id, wrapped := range set {
			waits := false
			for _, ns := range colocateTargets(wrapped.eval) {
				_, placed := namespaces[ns]
				waits = waits || placed
			}
			if !waits {
				continue
			}
			unblocked[wrapped.eval] = wrapped.token
			delete(set, id)
			delete(b.jobs, structs.NewNamespacedID(wrapped.eval.JobID, wrapped.eval.Namespace))
			if escaped {
				escapedUnblocked++
			}
			if wrapped.eval.QuotaLimitReached != "" {
				numQuotaLimit++
			}
		}
	}
	if len(unblocked) == 0 {
		return
	}
	b.stats.TotalEscaped -= escapedUnblocked
	b.stats.TotalQuotaLimit -= numQuotaLimit
	for eval := range unblocked {
		b.stats.Unblock(eval)
	}
	b.evalBroker.EnqueueAll(unblocked)
}

// missedAffinityUnblock reports whether an evaluation that waits for a
// required colocate rule was in the scheduler when a plan placed allocations
// in the namespace of the rule. It is called with the lock held.
func (b *BlockedEvals) missedAffinityUnblock(eval *structs.Evaluation) bool {
	for _, ns := range colocateTargets(eval) {
		if eval.SnapshotIndex < b.affinityIndexes[ns] {
			return true
		}
	}
	return false
}

// unblockAllocAffinityForPlanResults unblocks the evaluations waiting for a
// required colocate rule on the namespaces the plan placed allocations in.
func (n *nomadFSM) unblockAllocAffinityForPlanResults(req *structs.ApplyPlanResultsRequest, index uint64) {
	if !n.blockedEvals.Enabled() {
		return
	}
	namespaces := make(map[string]struct{})
	for _, list := range [][]*structs.Allocation{req.AllocsUpdated, req.Alloc} {
		for _, alloc := range list {
			if alloc != nil && !alloc.TerminalStatus() {
				namespaces[alloc.Namespace] = struct{}{}
			}
		}
	}
	n.blockedEvals.UnblockAllocAffinity(namespaces, index)
}
