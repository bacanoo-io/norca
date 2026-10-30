// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// applyAs applies a release with the given token.
func (c *releaseTestClient) applyAs(token, ns, name string, req *structs.ReleaseApplyRequest) (*structs.ReleaseApplyResponse, error) {
	req.Name = name
	req.WriteRequest = structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token}
	var resp structs.ReleaseApplyResponse
	err := msgpackrpc.CallWithCodec(c.codec, "Release.Apply", req, &resp)
	return &resp, err
}

// TestRelease_RecordOnlyAdoptionACL checks that a record-only apply, which
// adopts resources without writing them, needs the capabilities a real apply
// of each resource needs, and that taking resources over from a release
// needs release write in that release's namespace (PK-2).
func TestRelease_RecordOnlyAdoptionACL(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()
	must.NoError(t, store.UpsertNamespaces(900, []*structs.Namespace{{Name: "dev"}, {Name: "prod"}}))

	// prod/payments owns the job prod/payments; prod/orphan belongs to no
	// release.
	payments := testReleaseJob("payments")
	payments.Namespace = "prod"
	_, err := c.apply("prod", "payments", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "payments"},
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: payments}},
	})
	must.NoError(t, err)
	must.Len(t, 1, c.get("prod", "payments").Status.Resources)
	orphan := mock.Job()
	orphan.ID, orphan.Name, orphan.Namespace = "orphan", "orphan", "prod"
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 950, nil, orphan))

	jobRes := func(id string) []*structs.ReleaseResource {
		job := testReleaseJob(id)
		job.Namespace = "prod"
		return []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: job}}
	}
	devRelease := `namespace "dev" { object "packs.norca.io/Release" { capabilities = ["read", "write"] } }`

	// Release write in dev only: no adoption of prod's job, with or without
	// taking it over, and of the orphaned job.
	devOnly := mock.CreatePolicyAndToken(t, store, 1000, "dev-only", devRelease)
	_, err = c.applyAs(devOnly.SecretID, "dev", "evil", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "evil"}, Resources: jobRes("payments"), RecordOnly: true, TakeOwnership: true,
	})
	must.ErrorContains(t, err, "Permission denied")
	_, err = c.applyAs(devOnly.SecretID, "dev", "evil", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "evil"}, Resources: jobRes("orphan"), RecordOnly: true,
	})
	must.ErrorContains(t, err, "Permission denied")
	must.Len(t, 1, c.get("prod", "payments").Status.Resources)
	if rel := c.get("dev", "evil"); rel != nil {
		must.Len(t, 0, rel.Status.Resources)
	}

	// submit-job in prod allows adopting the orphaned job, but taking over
	// prod's release needs release write in prod as well.
	devAndProdJobs := mock.CreatePolicyAndToken(t, store, 1002, "dev-and-prod-jobs",
		devRelease+`
namespace "prod" { policy = "write" }`)
	_, err = c.applyAs(devAndProdJobs.SecretID, "dev", "evil", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "evil"}, Resources: jobRes("payments"), RecordOnly: true, TakeOwnership: true,
	})
	must.ErrorContains(t, err, `may not write releases in namespace "prod"`)
	must.Len(t, 1, c.get("prod", "payments").Status.Resources)

	resp, err := c.applyAs(devAndProdJobs.SecretID, "dev", "adopter", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "adopter"}, Resources: jobRes("orphan"), RecordOnly: true,
	})
	must.NoError(t, err)
	must.Len(t, 1, resp.Release.Status.Resources)

	// With release write in prod too, taking over is allowed.
	both := mock.CreatePolicyAndToken(t, store, 1004, "both",
		devRelease+`
namespace "prod" {
  policy = "write"
  object "packs.norca.io/Release" { capabilities = ["read", "write"] }
}`)
	_, err = c.applyAs(both.SecretID, "dev", "taker", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "taker"}, Resources: jobRes("payments"), RecordOnly: true, TakeOwnership: true,
	})
	must.NoError(t, err)
	must.Len(t, 0, c.get("prod", "payments").Status.Resources)
	must.Len(t, 1, c.get("dev", "taker").Status.Resources)

	// Global resources need what their endpoints need: management for ACL
	// policies.
	_, err = c.applyAs(both.SecretID, "dev", "policies", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "policies"}, RecordOnly: true,
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceACLPolicy, ACLPolicy: &structs.ACLPolicy{
			Name: "dev-only", Rules: devRelease,
		}}},
	})
	must.ErrorContains(t, err, "Permission denied")
}

// TestRelease_MultiregionAdoptionACL checks that a request describing a
// multi-region install, which makes this region adopt the multiregion jobs
// another region registers, needs submit-job for them (PK-2).
func TestRelease_MultiregionAdoptionACL(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()
	must.NoError(t, store.UpsertNamespaces(900, []*structs.Namespace{{Name: "dev"}, {Name: "prod"}}))

	job := mock.MultiregionJob()
	job.ID, job.Name, job.Namespace = "mr", "mr", "prod"
	job.Multiregion.Regions[0].Name = "other"
	job.Multiregion.Regions[1].Name = "global"
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 950, nil, job.Copy()))

	req := func() *structs.ReleaseApplyRequest {
		return &structs.ReleaseApplyRequest{
			Pack:        &structs.ReleasePack{Name: "mr"},
			Resources:   []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: job.Copy()}},
			Multiregion: &structs.ReleaseMultiregion{Regions: []string{"other", "global"}, GlobalRegion: "other"},
		}
	}

	devOnly := mock.CreatePolicyAndToken(t, store, 1000, "dev-only",
		`namespace "dev" { object "packs.norca.io/Release" { capabilities = ["read", "write"] } }`)
	_, err := c.applyAs(devOnly.SecretID, "dev", "evil", req())
	must.ErrorContains(t, err, "Permission denied")
	if rel := c.get("dev", "evil"); rel != nil {
		must.Len(t, 0, rel.Status.Resources)
	}

	// The submitter of the job may adopt it.
	resp, err := c.apply("dev", "mr", req())
	must.NoError(t, err)
	must.Len(t, 1, resp.Release.Status.Resources)
	must.Eq(t, structs.ReleaseChangeAdopt, resp.Changes[0].Action)
}
