// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// A release only purges a job under the ID of one of its hooks when the job
// is the hook of this release (review finding SRV-12): the ID of a hook job
// that is gone can be used by a job that has nothing to do with the release.
func TestRelease_HookJobOfAnotherOwnerIsKept(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	hook := func(phase string) *structs.ReleaseResource {
		job := mock.BatchJob()
		job.ID, job.Name, job.Namespace = "migrate", "migrate", ""
		job.Meta = map[string]string{
			structs.ReleaseMetaHook:        phase,
			structs.ReleaseMetaHookTimeout: "1s",
		}
		return &structs.ReleaseResource{Type: structs.ReleaseResourceJob, Job: job}
	}
	pack := func(phase string) *structs.ReleaseApplyRequest {
		return &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "app"}, Resources: []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceJob, Job: testReleaseJob("app-web")},
			hook(phase),
		}}
	}
	live := func() *structs.Job {
		job, err := c.srv.State().JobByID(nil, "default", "migrate")
		must.NoError(t, err)
		return job
	}

	// The hook can't run without nodes: the install fails, its revision is
	// kept, and the hook job is purged.
	both := structs.ReleaseHookPreInstall + "," + structs.ReleaseHookPreUpgrade
	_, err := c.apply("default", "app", pack(both))
	must.ErrorContains(t, err, "did not finish in time")
	must.Nil(t, live())

	// A hook job the release left behind is its own: the next run purges
	// and registers it again.
	kept := pack(both)
	kept.Resources[1].Job.Meta[structs.ReleaseMetaHookDelete] = structs.ReleaseHookDeleteNever
	_, err = c.apply("default", "app", kept)
	must.ErrorContains(t, err, "did not finish in time")
	must.Eq(t, "app", live().Meta[structs.ReleaseMetaRelease])
	first := live().CreateIndex
	_, err = c.apply("default", "app", kept)
	must.ErrorContains(t, err, "did not finish in time")
	must.Greater(t, first, live().CreateIndex)
	var dereg structs.JobDeregisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Job.Deregister", &structs.JobDeregisterRequest{
		JobID: "migrate", Purge: true,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &dereg))
	must.Nil(t, live())

	// A job of the same ID that does not belong to the release.
	unrelated := testReleaseJob("migrate")
	unrelated.Namespace = "default"
	var reg structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Job.Register", &structs.JobRegisterRequest{
		Job:          unrelated,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
	}, &reg))
	must.NotNil(t, live())

	// Running the hook again does not purge it either: the hook fails.
	_, err = c.apply("default", "app", pack(both))
	must.ErrorContains(t, err, "does not belong to the release")
	must.NotNil(t, live(), must.Sprint("the unrelated job after the hook ran again"))
	must.Eq(t, "", live().Meta[structs.ReleaseMetaRelease])

	_, err = c.uninstall("default", "app", "")
	must.NoError(t, err)
	must.Nil(t, c.get("default", "app"))
	must.NotNil(t, live(), must.Sprint("the unrelated job after the release was deleted"))
}
