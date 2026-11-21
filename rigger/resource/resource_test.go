// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resource_test

import (
	"slices"
	"testing"

	"github.com/bacanoo-io/norca/rigger/resource"
)

func TestBuiltin(t *testing.T) {
	t.Parallel()

	got := resource.Builtin()

	if got[0] != resource.KindNamespace || got[len(got)-1] != resource.KindJob {
		t.Fatalf("Builtin() = %v, want namespaces first and jobs last", got)
	}

	got[0] = "changed"

	if again := resource.Builtin(); again[0] != resource.KindNamespace {
		t.Errorf("Builtin() returned shared storage: a change by the caller is seen again (%q)", again[0])
	}

	if slices.Contains(resource.Builtin(), "") {
		t.Error("Builtin() holds an empty kind")
	}
}
