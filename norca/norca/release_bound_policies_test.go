// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// TestRelease_BoundPoliciesNeedTheirHolder checks the release path of review
// finding JOG-2: an operator installs a pack whose ACL policy is bound to the
// pack's controller job. The team of the namespace may upgrade the release
// as long as the job stays as it is, but a changed job runs under the
// operator's policy, so it needs a token that holds that policy.
func TestRelease_BoundPoliciesNeedTheirHolder(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	newForeignResources(t, c)
	store := c.srv.State()
	team := mock.CreatePolicyAndToken(t, store, 1000, "team-a", releaseTenantA)
	holder := mock.CreateToken(t, store, 1002, []string{"team-a", "op-ctrl"})

	pack := func(value string) []*structs.ReleaseResource {
		job := testReleaseJob("ctrl")
		job.TaskGroups[0].Tasks[0].Env["VERSION"] = value
		return []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{
				Name:   "op-ctrl",
				Rules:  `namespace "b" { policy = "write" }`,
				JobACL: &structs.JobACL{Namespace: "a", JobID: "ctrl"},
			}},
			{Type: structs.ReleaseResourceJob, Job: job},
		}
	}
	apply := func(token, value string) error {
		_, err := c.applyAs(token, "a", "op", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "op"}, Atomic: pointer.Of(true), Resources: pack(value),
		})
		return err
	}
	live := func() string {
		job, err := store.JobByID(nil, "a", "ctrl")
		must.NoError(t, err)
		must.NotNil(t, job)
		return job.TaskGroups[0].Tasks[0].Env["VERSION"]
	}

	// The operator installs the pack.
	must.NoError(t, apply(c.token, "1"))
	must.Eq(t, "1", live())

	// The team's upgrade with a changed job is refused.
	err := apply(team.SecretID, "team")
	must.ErrorContains(t, err, "Permission denied")
	must.ErrorContains(t, err, `"op-ctrl"`)
	must.Eq(t, "1", live())

	// The team's upgrade that leaves the job and the policy as they are
	// is not.
	must.NoError(t, apply(team.SecretID, "1"))
	must.Eq(t, structs.ReleasePhaseDeployed, c.get("a", "op").Status.Phase)
	must.Eq(t, "1", live())

	// A token that holds the bound policy may change the job.
	must.NoError(t, apply(holder.SecretID, "2"))
	must.Eq(t, "2", live())

	// A rollback writes the job of an earlier revision: the same rule.
	rollback := func(token string, revision uint64) error {
		return c.srv.RPC("Release.Rollback", &structs.ReleaseRollbackRequest{
			Name: "op", Revision: revision, Atomic: pointer.Of(true),
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "a", AuthToken: token},
		}, new(structs.ReleaseApplyResponse))
	}
	err = rollback(team.SecretID, 1)
	must.ErrorContains(t, err, `"op-ctrl"`)
	must.Eq(t, "2", live())
	must.NoError(t, rollback(holder.SecretID, 1))
	must.Eq(t, "1", live())
}
