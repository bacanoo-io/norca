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
		Owner:   stackNamespace + "/platform",
		Adopt:   true,
	}

	out, err := applyRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if out.Pack.Manager != api.ReleaseManagerRigger || out.InputDigest != "sha256:release" || out.ValuesHash != "" || !out.Adopt {
		t.Errorf("manager %q, input digest %q, values hash %q, adopt %v", out.Pack.Manager, out.InputDigest, out.ValuesHash, out.Adopt)
	}

	want := api.ReleaseOwner{Kind: api.ReleaseOwnerKindStack, Namespace: stackNamespace, Name: "platform"}
	if out.Owner == nil || *out.Owner != want {
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

// shop is the stack of the tests of ownerOf, stackNamespace its namespace.
const (
	shop           = "shop"
	stackNamespace = "apps"
)

// The owner is the field Owner, by namespace and name; a description is
// not read. The digest of a revision without InputDigest is its ValuesHash.
func TestOwnerOfAndRevision(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		owner *api.ReleaseOwner
		want  string
	}{
		{"stack", &api.ReleaseOwner{Kind: api.ReleaseOwnerKindStack, Namespace: stackNamespace, Name: shop}, stackNamespace + "/" + shop},
		{"installed alone", nil, ""},
		{"another kind", &api.ReleaseOwner{Kind: "other", Namespace: stackNamespace, Name: "x"}, ""},
	}
	for _, tc := range cases {
		if got := ownerOf(tc.owner); got != tc.want {
			t.Errorf("%s: owner %q, want %q", tc.name, got, tc.want)
		}
	}

	ref := host.ReleaseRef{Namespace: "default", Name: "web"}

	old := revision(ref, &api.ReleaseRevision{Revision: 1, Spec: &api.ReleaseRevisionSpec{ValuesHash: "sha256:old", Description: "rigger stack shop"}})
	if old.Digest != "sha256:old" || old.Owner != "" {
		t.Errorf("a revision without the fields: digest %q, owner %q", old.Digest, old.Owner)
	}

	now := revision(ref, &api.ReleaseRevision{Revision: 2, Spec: &api.ReleaseRevisionSpec{
		ValuesHash: "sha256:pack", InputDigest: "sha256:new",
		Owner: &api.ReleaseOwner{Kind: api.ReleaseOwnerKindStack, Namespace: stackNamespace, Name: shop},
	}})
	if now.Digest != "sha256:new" || now.Owner != stackNamespace+"/"+shop {
		t.Errorf("digest %q, owner %q", now.Digest, now.Owner)
	}
}
