// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// The tests of this file state the owner, the manager and the input digest
// of a release (design/rig-integration.md 4.4 a-c).

func (c *releaseTestClient) list(ns, owner string) []*structs.Release {
	req := &structs.ReleaseListRequest{Owner: owner,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.ReleaseListResponse
	must.NoError(c.t, msgpackrpc.CallWithCodec(c.codec, "Release.List", req, &resp))
	return resp.Releases
}

func (c *releaseTestClient) rollbackAdopt(ns, name string, rev uint64, adopt bool) (*structs.ReleaseApplyResponse, error) {
	req := &structs.ReleaseRollbackRequest{Name: name, Revision: rev, Adopt: adopt,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: c.token}}
	var resp structs.ReleaseApplyResponse
	err := msgpackrpc.CallWithCodec(c.codec, "Release.Rollback", req, &resp)
	return &resp, err
}

func ownedApply(stack, version, manager, digest string) *structs.ReleaseApplyRequest {
	req := &structs.ReleaseApplyRequest{
		Pack:        &structs.ReleasePack{Name: "web", Version: version, Manager: manager},
		Resources:   []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("owned-web")}},
		InputDigest: digest,
	}
	if stack != "" {
		req.Owner = &structs.ReleaseOwner{Kind: structs.ReleaseOwnerKindStack, Namespace: "default", Name: stack, Version: "1.0.0", Part: "front"}
	}
	return req
}

// The owner and the input digest are stored in the release and the
// revision, a rollback restores those of its revision, and the list filters
// by owner.
func TestRelease_OwnerAndInputDigest(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	_, err := c.apply("default", "web", ownedApply("shop", "1.0.0", structs.ReleaseManagerRigger, "sha256:one"))
	must.NoError(t, err)
	_, err = c.apply("default", "other", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "other", Version: "1.0.0"},
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("other-web")}},
	})
	must.NoError(t, err)

	rel := c.get("default", "web")
	must.Eq(t, &structs.ReleaseOwner{Kind: "stack", Namespace: "default", Name: "shop", Version: "1.0.0", Part: "front"}, rel.Spec.Owner)
	must.Eq(t, structs.ReleaseManagerRigger, rel.Spec.Pack.Manager)
	rev := c.revision("default", "web", 1)
	must.Eq(t, "sha256:one", rev.Spec.InputDigest)
	must.Eq(t, "shop", rev.Spec.Owner.Name)

	// A release without a manager is the pack manager's, written so.
	must.Eq(t, structs.ReleaseManagerPack, c.get("default", "other").Spec.Pack.Manager)
	must.Nil(t, c.get("default", "other").Spec.Owner)

	// The list filters by owner.
	owned := c.list("default", "stack/default/shop")
	must.Len(t, 1, owned)
	must.Eq(t, "web", owned[0].Name)
	must.Len(t, 0, c.list("default", "stack/default/other-shop"))
	must.Len(t, 2, c.list("default", ""))

	// An upgrade by another stack and digest, which adopts the release, then
	// a rollback that does, restores the owner and the digest of revision 1.
	byMall := ownedApply("mall", "1.1.0", structs.ReleaseManagerRigger, "sha256:two")
	byMall.Adopt = true
	_, err = c.apply("default", "web", byMall)
	must.NoError(t, err)
	must.Eq(t, "mall", c.get("default", "web").Spec.Owner.Name)
	_, err = c.rollbackAdopt("default", "web", 1, true)
	must.NoError(t, err)
	rel = c.get("default", "web")
	must.Eq(t, "shop", rel.Spec.Owner.Name)
	must.Eq(t, "sha256:one", c.revision("default", "web", rel.Spec.Revision).Spec.InputDigest)

	// An owner of an unknown kind, or without a name, is refused.
	bad := ownedApply("shop", "1.2.0", structs.ReleaseManagerRigger, "")
	bad.Owner.Kind = "module"
	_, err = c.apply("default", "web", bad)
	must.Eq(t, 400, errCode(t, err))
	bad.Owner = &structs.ReleaseOwner{Kind: structs.ReleaseOwnerKindStack, Name: "shop"}
	_, err = c.apply("default", "web", bad)
	must.Eq(t, 400, errCode(t, err), must.Sprint("no namespace"))
	bad.Owner = &structs.ReleaseOwner{Kind: structs.ReleaseOwnerKindStack}
	_, err = c.apply("default", "web", bad)
	must.Eq(t, 400, errCode(t, err))
}

// One release has one manager: the other one takes it over only with Adopt,
// in an apply and in a rollback; an unknown manager is refused.
func TestRelease_OneManager(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	// Installed by the pack manager (no manager given).
	_, err := c.apply("default", "web", ownedApply("", "1.0.0", "", ""))
	must.NoError(t, err)

	// Rigger is refused without adopt, in a dry run too, and nothing changes.
	_, err = c.apply("default", "web", ownedApply("shop", "2.0.0", structs.ReleaseManagerRigger, ""))
	must.Eq(t, 409, errCode(t, err))
	must.ErrorContains(t, err, `release "web" is managed by pack: rigger takes it over only when asked to`)
	dry := ownedApply("shop", "2.0.0", structs.ReleaseManagerRigger, "")
	dry.DryRun = true
	_, err = c.apply("default", "web", dry)
	must.Eq(t, 409, errCode(t, err))
	must.Eq(t, uint64(1), c.get("default", "web").Spec.Revision)

	// With adopt it takes the release over; then the pack manager is the one
	// refused.
	adopt := ownedApply("shop", "2.0.0", structs.ReleaseManagerRigger, "")
	adopt.Adopt = true
	_, err = c.apply("default", "web", adopt)
	must.NoError(t, err)
	must.Eq(t, structs.ReleaseManagerRigger, c.get("default", "web").Spec.Pack.Manager)
	_, err = c.apply("default", "web", ownedApply("", "3.0.0", structs.ReleaseManagerPack, ""))
	must.Eq(t, 409, errCode(t, err))
	must.ErrorContains(t, err, "is managed by rigger: pack takes it over")

	// A rollback to the revision of the pack manager is a takeover too.
	_, err = c.rollbackAdopt("default", "web", 1, false)
	must.Eq(t, 409, errCode(t, err))
	_, err = c.rollbackAdopt("default", "web", 1, true)
	must.NoError(t, err)
	must.Eq(t, structs.ReleaseManagerPack, c.get("default", "web").Spec.Pack.Manager)

	// The same manager needs no adopt.
	_, err = c.apply("default", "web", ownedApply("", "4.0.0", structs.ReleaseManagerPack, ""))
	must.NoError(t, err)

	unknown := ownedApply("", "5.0.0", "helm", "")
	_, err = c.apply("default", "web", unknown)
	must.Eq(t, 400, errCode(t, err))
	must.ErrorContains(t, err, `unknown manager "helm"`)
}
