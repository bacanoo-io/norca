// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// rollbackKeepsScaledCounts returns the job a deployment rolls back to with the
// counts of the stored job kept for the task groups that an enabled scaling
// policy scales in both (review finding ASC-4).
//
// The rollback of a failed deployment writes a stable version of the job as
// it was stored, with the count of that version. A count that a policy
// scaled meanwhile, during the failed deployment too (the built-in engine
// scales up during one), was lost: the allocations stopped without a
// scaling event or a cooldown, under load. A new version registered through
// the endpoint keeps such counts (keepScaledCounts of the job endpoint); the
// rollback is written by the deployment watcher and by Deployment.Fail
// straight into the state, so it is kept here, at the index the rollback is
// applied at, brought within the bounds of the policy of the version rolled
// back to.
func rollbackKeepsScaledCounts(txn Txn, job *structs.Job) (*structs.Job, error) {
	raw, err := txn.First("jobs", "id", job.Namespace, job.ID)
	if err != nil || raw == nil {
		return job, err
	}
	stored := raw.(*structs.Job)
	var out *structs.Job
	for i, tg := range job.TaskGroups {
		old := stored.LookupTaskGroup(tg.Name)
		if old == nil || !scaledByPolicy(old) || !scaledByPolicy(tg) {
			continue
		}
		count := int(min(max(int64(old.Count), tg.Scaling.Min), tg.Scaling.Max))
		if count == tg.Count {
			continue
		}
		if out == nil {
			out = job.Copy()
		}
		out.TaskGroups[i].Count = count
	}
	if out == nil {
		return job, nil
	}
	return out, nil
}

// scaledByPolicy reports whether an enabled horizontal scaling policy scales
// the task group (isScaledGroup of the job endpoint).
func scaledByPolicy(tg *structs.TaskGroup) bool {
	p := tg.Scaling
	return p != nil && p.Enabled && (p.Type == "" || p.Type == structs.ScalingPolicyTypeHorizontal)
}
