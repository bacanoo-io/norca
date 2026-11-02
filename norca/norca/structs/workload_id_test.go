// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestWorkloadIdentity_Equal(t *testing.T) {
	ci.Parallel(t)

	var orig *WorkloadIdentity

	newWI := orig.Copy()
	must.Equal(t, orig, newWI)

	orig = &WorkloadIdentity{}
	must.NotEqual(t, orig, newWI)

	newWI = &WorkloadIdentity{}
	must.Equal(t, orig, newWI)

	orig.Env = true
	must.NotEqual(t, orig, newWI)

	newWI.Env = true
	must.Equal(t, orig, newWI)

	newWI.File = true
	must.NotEqual(t, orig, newWI)
}
