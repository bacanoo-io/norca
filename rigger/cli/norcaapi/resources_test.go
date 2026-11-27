// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"slices"
	"testing"

	"github.com/bacanoo-io/norca/rigger/resource"
)

// Kinds are what an engine of a Norca host takes: exactly the kinds the
// release API has a payload for, in the order the server applies them.
func TestKinds(t *testing.T) {
	t.Parallel()

	kinds := Kinds()

	if len(kinds) != len(payloads) {
		t.Errorf("Kinds() = %v, want the %d kinds of the payloads", kinds, len(payloads))
	}

	for _, k := range kinds {
		if _, ok := payloads[k]; !ok {
			t.Errorf("Kinds() has %s, which the release API has no payload for", k)
		}
	}

	order := resource.Builtin()
	if !slices.IsSortedFunc(kinds, func(a, b resource.Kind) int { return slices.Index(order, a) - slices.Index(order, b) }) {
		t.Errorf("Kinds() = %v, want them in the order %v", kinds, order)
	}

	if slices.Contains(kinds, "audit") {
		t.Error("Kinds() has audit, which the release API does not apply")
	}
}
