// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// A set with an unknown item has no known size: there is nothing to check
// until it is known.
func TestChecker_UnknownInSet(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()

	one := 1
	k := &checker{}
	k.check(&lang.Constraint{MinItems: &one}, cty.SetVal([]cty.Value{cty.UnknownVal(cty.String)}), "var.s")

	if k.diags.HasErrors() {
		t.Errorf("check() = %v, want nothing of a value not known", k.diags)
	}
}
