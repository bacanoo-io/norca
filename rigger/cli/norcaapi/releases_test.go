// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca/rigger/host"
)

// A request of the engine becomes a release request of the manager rigger,
// with the stack in Owner and the digest in InputDigest; adopt is passed on.
func TestApplyRequest_OwnerManagerDigest(t *testing.T) {
	t.Parallel()

	req := host.ApplyRequest{
		Release: host.ReleaseRef{Namespace: "default", Name: "web"},
		Module:  host.ModuleRef{Name: "web", Version: "0.1.0", Digest: "sha256:module"},
		Digest:  "sha256:release",
		Owner:   "platform",
		Adopt:   true,
	}

	out, err := applyRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if out.Pack.Manager != api.ReleaseManagerRigger || out.InputDigest != "sha256:release" || out.ValuesHash != "" || !out.Adopt {
		t.Errorf("manager %q, input digest %q, values hash %q, adopt %v", out.Pack.Manager, out.InputDigest, out.ValuesHash, out.Adopt)
	}

	if out.Owner == nil || out.Owner.Kind != api.ReleaseOwnerKindStack || out.Owner.Name != "platform" {
		t.Errorf("owner %+v", out.Owner)
	}

	req.Owner, req.Adopt = "", false

	out, err = applyRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if out.Owner != nil || out.Description != ownDesc || out.Adopt {
		t.Errorf("a release of no stack: owner %+v, description %q, adopt %v", out.Owner, out.Description, out.Adopt)
	}
}

// The owner is the field Owner; a release written before it has the owner
// in its description, and the digest of such a revision is its ValuesHash.
func TestOwnerOfAndRevision(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		owner       *api.ReleaseOwner
		description string
		want        string
	}{
		{"field", &api.ReleaseOwner{Kind: api.ReleaseOwnerKindStack, Name: "shop"}, "rigger stack other", "shop"},
		{"description before the field", nil, "rigger stack shop", "shop"},
		{"installed alone", nil, "rigger", ""},
		{"another kind", &api.ReleaseOwner{Kind: "other", Name: "x"}, "", ""},
	}
	for _, tc := range cases {
		if got := ownerOf(tc.owner, tc.description); got != tc.want {
			t.Errorf("%s: owner %q, want %q", tc.name, got, tc.want)
		}
	}

	ref := host.ReleaseRef{Namespace: "default", Name: "web"}

	old := revision(ref, &api.ReleaseRevision{Revision: 1, Spec: &api.ReleaseRevisionSpec{ValuesHash: "sha256:old", Description: "rigger stack shop"}})
	if old.Digest != "sha256:old" || old.Owner != "shop" {
		t.Errorf("a revision written before the fields: digest %q, owner %q", old.Digest, old.Owner)
	}

	now := revision(ref, &api.ReleaseRevision{Revision: 2, Spec: &api.ReleaseRevisionSpec{
		ValuesHash: "sha256:pack", InputDigest: "sha256:new",
		Owner: &api.ReleaseOwner{Kind: api.ReleaseOwnerKindStack, Name: "shop"},
	}})
	if now.Digest != "sha256:new" || now.Owner != "shop" {
		t.Errorf("digest %q, owner %q", now.Digest, now.Owner)
	}
}
