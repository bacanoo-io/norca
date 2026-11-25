// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The release kinds share the group rigger.norca.io with the kinds of the
// registries and the operations of Rigger. A release and its revisions are
// written by the release API alone, also by a management token; a registry
// of the same group is written through the object API.
func TestReleaseKinds_OnlyTheReleaseAPIWritesThem(t *testing.T) {
	ci.Parallel(t)

	r := newRegistryTest(t)
	repo := registryRepo(t)
	write := structs.WriteRequest{Region: "global", AuthToken: r.token}

	for _, kind := range []string{structs.ReleaseKind, structs.ReleaseRevisionKind} {
		var resp structs.CustomObjectWriteResponse

		err := r.s.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
			Object: &structs.CustomObject{
				Group: structs.ReleaseObjectGroup, Kind: kind, Namespace: structs.DefaultNamespace, Name: "forged",
				Spec: map[string]any{},
			},
			WriteRequest: write,
		}, &resp)
		must.ErrorContains(t, err, "managed by the release API", must.Sprint(kind))

		var deleted structs.GenericResponse

		err = r.s.RPC("ObjectKind.DeleteObjectKind", &structs.ObjectKindDeleteRequest{
			Group: structs.ReleaseObjectGroup, Kind: kind, WriteRequest: write,
		}, &deleted)
		must.ErrorContains(t, err, "managed by the release API", must.Sprint(kind))
	}

	must.NoError(t, r.apply("acme", map[string]any{"type": "git", "source": repo.dir}))
}

func TestIsReleaseObjectKind(t *testing.T) {
	ci.Parallel(t)

	must.True(t, isReleaseObjectKind(structs.ReleaseObjectGroup, structs.ReleaseKind))
	must.True(t, isReleaseObjectKind(structs.ReleaseObjectGroup, structs.ReleaseRevisionKind))
	must.False(t, isReleaseObjectKind(structs.RiggerObjectGroup, structs.RiggerRegistryKind))
	must.False(t, isReleaseObjectKind(structs.RiggerObjectGroup, structs.RiggerOperationKind))
	must.False(t, isReleaseObjectKind("example.org", structs.ReleaseKind))
}
