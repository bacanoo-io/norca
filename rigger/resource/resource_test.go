// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resource_test

import (
	"slices"
	"testing"

	"github.com/bacanoo-io/norca/rigger/resource"
)

// The built-in kinds are in the order the servers of Norca apply the
// resource types of a release (releaseResourceOrder of norca): a quota
// before the namespaces, so that a namespace is written with the quota it
// names, and the jobs last.
func TestBuiltin(t *testing.T) {
	t.Parallel()

	got := resource.Builtin()

	want := []resource.Kind{
		resource.KindQuota, resource.KindNamespace, resource.KindACLPolicy, resource.KindVariable, resource.KindObjectKind,
		resource.KindAdmissionPolicy, resource.KindExternalService, resource.KindObject, resource.KindJob,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Builtin() = %v, want %v", got, want)
	}

	got[0] = "changed"

	if again := resource.Builtin(); again[0] != resource.KindQuota {
		t.Errorf("Builtin() returned shared storage: a change by the caller is seen again (%q)", again[0])
	}

	if slices.Contains(resource.Builtin(), "") {
		t.Error("Builtin() holds an empty kind")
	}
}
