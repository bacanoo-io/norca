// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// checkJobParent refuses a registration that sets the parent of a job
// (review finding JOG-7). The parent decides the workload identity of the
// job's allocations and the ACL policies bound to them, so only the servers
// set it, when they dispatch a parameterized job or launch a periodic one;
// those paths write the child through Raft, not through Job.Register. A new
// version of an existing child (an update, a revert, a scaling) keeps the
// parent the child has. Refused rather than cleared: a cleared parent would
// silently give the job another identity than the one the caller sent.
func checkJobParent(job, existing *structs.Job) error {
	if job.ParentID == "" {
		return nil
	}
	if existing != nil && existing.ParentID == job.ParentID {
		return nil
	}
	return structs.NewErrRPCCodedf(http.StatusBadRequest,
		"the parent of job %q can't be set: only the servers set it when they dispatch or launch a child job", job.ID)
}
