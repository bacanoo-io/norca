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
	applyReq := func(token, value string, dryRun bool) (*structs.ReleaseApplyResponse, error) {
		return c.applyAs(token, "a", "op", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "op"}, Atomic: pointer.Of(true), DryRun: dryRun, Resources: pack(value),
		})
	}
	apply := func(token, value string) error {
		_, err := applyReq(token, value, false)
		return err
	}
	live := func() string {
		job, err := store.JobByID(nil, "a", "ctrl")
		must.NoError(t, err)
		must.NotNil(t, job)
		return job.TaskGroups[0].Tasks[0].Env["VERSION"]
	}
	// refused checks a refusal of the release permission check: a 403 (the
	// HTTP layer answers with the code of an RPC coded error) that names the
	// policy, before anything is applied or recorded.
	refused := func(err error, version string) {
		t.Helper()
		must.Error(t, err)
		code, msg, ok := structs.CodeFromRPCCodedErr(err)
		must.True(t, ok, must.Sprint(err))
		must.Eq(t, 403, code)
		must.StrContains(t, msg, "Permission denied")
		must.StrContains(t, msg, `"op-ctrl"`)
		must.Eq(t, version, live())
	}

	// The operator installs the pack.
	must.NoError(t, apply(c.token, "1"))
	must.Eq(t, "1", live())
	rel := c.get("a", "op")
	revisions := len(c.history("a", "op"))

	// The team's upgrade with a changed job is refused before anything is
	// applied or recorded: no revision, the release as it was.
	refused(apply(team.SecretID, "team"), "1")
	must.Len(t, revisions, c.history("a", "op"))
	after := c.get("a", "op")
	must.Eq(t, rel.Status.Revision, after.Status.Revision)
	must.Eq(t, rel.Spec.Revision, after.Spec.Revision)
	must.Eq(t, structs.ReleasePhaseDeployed, after.Status.Phase)

	// The plan of that upgrade says so for the job.
	plan, err := applyReq(team.SecretID, "team", true)
	must.NoError(t, err)
	var jobChange *structs.ReleaseResourceChange
	for _, ch := range plan.Changes {
		if ch.Type == structs.ReleaseResourceJob {
			jobChange = ch
		}
	}
	must.NotNil(t, jobChange)
	must.Eq(t, structs.ReleaseChangeUpdate, jobChange.Action)
	must.StrContains(t, jobChange.Error, "Permission denied")
	must.StrContains(t, jobChange.Error, `"op-ctrl"`)

	// The team's upgrade that leaves the job and the policy as they are
	// is not refused, and its plan has no error.
	plan, err = applyReq(team.SecretID, "1", true)
	must.NoError(t, err)
	for _, ch := range plan.Changes {
		must.Eq(t, "", ch.Error, must.Sprint(ch.Type))
	}
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
	revisions = len(c.history("a", "op"))
	refused(rollback(team.SecretID, 1), "2")
	must.Len(t, revisions, c.history("a", "op"))
	must.NoError(t, rollback(holder.SecretID, 1))
	must.Eq(t, "1", live())

	// Job.Register stays the backstop: the team can't change the job
	// outside the release either.
	job, err := store.JobByID(nil, "a", "ctrl")
	must.NoError(t, err)
	changed := job.Copy()
	changed.TaskGroups[0].Tasks[0].Env["VERSION"] = "direct"
	err = c.srv.RPC("Job.Register", &structs.JobRegisterRequest{
		Job:          changed,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "a", AuthToken: team.SecretID},
	}, new(structs.JobRegisterResponse))
	refused(err, "1")
}
