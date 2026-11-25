// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/shoenig/test/must"
)

// The release kinds are of the group of Rigger, rigger.norca.io, next to
// the kinds of its registries and operations.
func TestReleaseObjectGroup_IsTheGroupOfRigger(t *testing.T) {
	must.Eq(t, "rigger.norca.io", ReleaseObjectGroup)
	must.Eq(t, RiggerObjectGroup, ReleaseObjectGroup)
	must.Eq(t, "rigger.norca.io/Release", ReleaseKindRef)
	must.Eq(t, "rigger.norca.io/ReleaseRevision", ReleaseRevisionKindRef)
	must.Eq(t, "rigger.norca.io/release", ReleaseLabel)
}

// A release holds no release and no revision, and defines neither kind: the
// release API alone writes them. The other kinds of the group are resources
// like any other: a module may register a registry of Rigger.
func TestReleaseResource_Validate_KindsOfTheReleaseGroup(t *testing.T) {
	object := func(kind string) *ReleaseResource {
		return &ReleaseResource{Type: ReleaseResourceObject, Object: &CustomObject{Group: ReleaseObjectGroup, Kind: kind, Name: "x"}}
	}
	objectKind := func(kind string) *ReleaseResource {
		return &ReleaseResource{Type: ReleaseResourceObjectKind, ObjectKind: &ObjectKind{Group: ReleaseObjectGroup, Kind: kind}}
	}

	for _, kind := range []string{ReleaseKind, ReleaseRevisionKind} {
		must.ErrorContains(t, object(kind).Validate(), "can't be part of a release", must.Sprint(kind))
		must.ErrorContains(t, objectKind(kind).Validate(), "is built in", must.Sprint(kind))
	}

	must.NoError(t, object(RiggerRegistryKind).Validate())
	must.NoError(t, objectKind("Widget").Validate())
}
