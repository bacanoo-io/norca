// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestReleaseMultiregion_Validate(t *testing.T) {
	mr := &ReleaseMultiregion{Regions: []string{"eu-aws", "eu-gcp"}, GlobalRegion: "eu-aws"}
	must.NoError(t, mr.Validate("eu-aws"))
	must.NoError(t, mr.Validate("eu-gcp"))
	must.ErrorContains(t, mr.Validate("us"), "not one of the regions")

	bad := &ReleaseMultiregion{Regions: []string{"eu-aws", "eu-gcp"}, GlobalRegion: "eu-gcp"}
	must.ErrorContains(t, bad.Validate("eu-gcp"), "must be the first region")

	dup := &ReleaseMultiregion{Regions: []string{"a", "a"}, GlobalRegion: "a"}
	must.ErrorContains(t, dup.Validate("a"), "duplicate")

	must.Error(t, (&ReleaseMultiregion{}).Validate("a"))
}

func TestReleaseMultiregion_JobSubmitter(t *testing.T) {
	mr := &ReleaseMultiregion{Regions: []string{"global", "eu", "us"}, GlobalRegion: "global"}

	// The first region of the install the job lists submits it.
	must.Eq(t, "eu", mr.JobSubmitter([]string{"us", "eu"}))
	must.Eq(t, "global", mr.JobSubmitter([]string{"eu", "global"}))
	// A job that lists none of the regions is submitted by the global region.
	must.Eq(t, "global", mr.JobSubmitter([]string{"asia"}))
}

func TestIsGlobalReleaseResource(t *testing.T) {
	for _, typ := range []string{ReleaseResourceNamespace, ReleaseResourceQuota, ReleaseResourceACLPolicy} {
		must.True(t, IsGlobalReleaseResource(typ), must.Sprint(typ))
	}
	for _, typ := range []string{ReleaseResourceVariable, ReleaseResourceObjectKind, ReleaseResourceAdmissionPolicy,
		ReleaseResourceObject, ReleaseResourceJob} {
		must.False(t, IsGlobalReleaseResource(typ), must.Sprint(typ))
	}
}
