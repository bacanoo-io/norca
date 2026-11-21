// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// The tests of this file state what it takes to mark a job as a job of a
// release (the meta pack.release and pack.release_namespace; review finding
// JOG-23): the release operations take a job with the marks as their own
// (its hooks, the jobs a failed apply left, the hook jobs a deletion
// removes), so the right to submit jobs must not be enough to put them on.

// releaseMeta is the marks of a job of the release namespace/name.
func releaseMeta(namespace, name string) map[string]string {
	return map[string]string{
		structs.ReleaseMetaRelease:          name,
		structs.ReleaseMetaReleaseNamespace: namespace,
	}
}

const releaseMarkSubmitter = `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}
namespace "other" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`

// The releases of "default" written, and the jobs of both namespaces.
const releaseMarkReleaser = `namespace "default" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
  object "packs.norca.io/Release" { capabilities = ["read", "write"] }
}
namespace "other" {
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`

func TestJobReleaseMark_NeedsWriteOnReleases(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "submitter", releaseMarkSubmitter))
	releaser := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "releaser", releaseMarkReleaser))
	finalizers := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1005, "finalizers", `namespace "default" {
  capabilities = ["submit-job"]
  object "packs.norca.io/Release" { capabilities = ["manage-finalizers"] }
}`))

	// Without "write" on the releases of the namespace the marks name, the
	// marks are refused and no job is written.
	err := submitter.registerJob(markedJob("default", "hook", releaseMeta("default", "app")))
	must.Eq(t, 403, errCode(t, err), must.Sprint("the release marks were accepted with submit-job only"))
	must.ErrorContains(t, err, `release "app" in namespace "default"`)
	must.ErrorContains(t, err, "require the write capability on packs.norca.io/Release")

	// The two marks go together; one alone is refused, whatever the rights.
	for _, meta := range []map[string]string{
		{structs.ReleaseMetaRelease: "app"},
		{structs.ReleaseMetaReleaseNamespace: "default"},
	} {
		err := releaser.registerJob(markedJob("default", "hook", meta))
		must.Eq(t, 400, errCode(t, err))
		must.ErrorContains(t, err, "go together")
	}
	err = finalizers.registerJob(markedJob("default", "hook", releaseMeta("default", "app")))
	must.Eq(t, 403, errCode(t, err), must.Sprint("manage-finalizers is not write"))

	// The releases of "default" do not give the marks of another namespace,
	// on a job of either namespace.
	err = releaser.registerJob(markedJob("default", "hook", releaseMeta("other", "theirs")))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, `release "theirs" in namespace "other"`)
	err = releaser.registerJob(markedJob("other", "hook", releaseMeta("other", "theirs")))
	must.Eq(t, 403, errCode(t, err))

	job, err := c.s.State().JobByID(nil, "default", "hook")
	must.NoError(t, err)
	must.Nil(t, job)

	// With "write" on the releases the marks are written, also on a job of
	// another namespace.
	must.NoError(t, releaser.registerJob(markedJob("default", "hook", releaseMeta("default", "app"))))
	must.NoError(t, releaser.registerJob(markedJob("other", "across", releaseMeta("default", "app"))))
	must.Eq(t, "app", c.jobIn("default", "hook").Meta[structs.ReleaseMetaRelease])

	// A job without the marks takes no right on releases.
	must.NoError(t, submitter.registerJob(markedJob("default", "plain", map[string]string{"team": "a"})))
}

// Whoever may update a marked job keeps its marks without a right on the
// releases; changing them takes the right, removing them does not.
func TestJobReleaseMark_UpdateOfAMarkedJob(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "submitter", releaseMarkSubmitter))
	releaser := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "releaser", releaseMarkReleaser))
	must.NoError(t, releaser.registerJob(markedJob("default", "web", releaseMeta("default", "app"))))

	// The same marks: an update of the job by its team.
	kept := markedJob("default", "web", releaseMeta("default", "app"))
	kept.Priority = 70
	must.NoError(t, submitter.registerJob(kept))
	must.Eq(t, 70, c.jobIn("default", "web").Priority)

	// Another release of the same namespace: refused.
	err := submitter.registerJob(markedJob("default", "web", releaseMeta("default", "other-app")))
	must.Eq(t, 403, errCode(t, err))
	must.Eq(t, "app", c.jobIn("default", "web").Meta[structs.ReleaseMetaRelease])

	// Without the marks: the job is no longer the release's.
	must.NoError(t, submitter.registerJob(markedJob("default", "web", map[string]string{})))
	_, marked := c.jobIn("default", "web").Meta[structs.ReleaseMetaRelease]
	must.False(t, marked)

	// Putting them back is a new mark.
	err = submitter.registerJob(markedJob("default", "web", releaseMeta("default", "app")))
	must.Eq(t, 403, errCode(t, err))
}

// A stopped job holds no marks to keep: registering it again with them is a
// new mark (as for the owner mark, review finding JOG-24).
func TestJobReleaseMark_StoppedJobHoldsNoMark(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := jobOwnerMarkServer(t)
	defer cleanup()

	submitter := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "submitter", releaseMarkSubmitter))
	releaser := c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "releaser", releaseMarkReleaser))
	must.NoError(t, releaser.registerJob(markedJob("default", "web", releaseMeta("default", "app"))))

	var resp structs.JobDeregisterResponse
	must.NoError(t, c.s.RPC("Job.Deregister", &structs.JobDeregisterRequest{
		JobID: "web", Purge: false, WriteRequest: c.write("default"),
	}, &resp))
	must.True(t, c.jobIn("default", "web").Stop)

	err := submitter.registerJob(markedJob("default", "web", releaseMeta("default", "app")))
	must.Eq(t, 403, errCode(t, err), must.Sprint("a stopped job gave its marks to a caller without the right"))
}

// A dispatch can't set the marks, and a parameterized job can't list them.
func TestJobReleaseMark_Dispatch(t *testing.T) {
	ci.Parallel(t)
	for _, key := range []string{structs.ReleaseMetaRelease, structs.ReleaseMetaReleaseNamespace} {
		err := checkDispatchReservedMeta(map[string]string{key: "app"})
		must.ErrorContains(t, err, "is reserved: a dispatch can't set it")

		job := mock.BatchJob()
		job.ParameterizedJob = &structs.ParameterizedJobConfig{MetaOptional: []string{key}}
		_, err = jobReservedMetaHook{}.Validate(job)
		must.ErrorContains(t, err, "is reserved")
	}
}
