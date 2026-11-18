// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The ACL policies bound to a job are those bound to its exact ID: the
// policy of the job "ctrl" is not granted to the allocations of a job whose
// ID is a prefix of it, such as "ctr" (found with review finding JOG-2).
func TestResolveClaims_BoundPolicyExactJobID(t *testing.T) {
	ci.Parallel(t)
	srv, _, cleanup := TestACLServer(t, nil)
	defer cleanup()
	store := srv.fsm.State()

	alloc := mock.Alloc()
	alloc.Job.ID = "ctr"
	alloc.JobID = alloc.Job.ID
	claims := &structs.IdentityClaims{
		Namespace:    alloc.Namespace,
		JobID:        alloc.Job.ID,
		AllocationID: alloc.ID,
		TaskName:     alloc.Job.TaskGroups[0].Tasks[0].Name,
	}
	policy := mock.ACLPolicy()
	policy.JobACL = &structs.JobACL{Namespace: alloc.Namespace, JobID: "ctrl"}
	policy.SetHash()
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 100, []*structs.Allocation{alloc}))
	must.NoError(t, store.UpsertACLPolicies(structs.MsgTypeTestSetup, 101, []*structs.ACLPolicy{policy}))

	policies, err := srv.resolvePoliciesForClaims(claims)
	must.NoError(t, err)
	must.SliceEmpty(t, policies)
}
