// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"

	"github.com/bacanoo-io/norca/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
)

func TestCompose_Constraints(t *testing.T) {
	testutil.Parallel(t)

	c := NewConstraint("kernel.name", "=", "darwin")
	expect := &Constraint{
		LTarget: "kernel.name",
		RTarget: "darwin",
		Operand: "=",
	}
	must.Eq(t, expect, c)
}
