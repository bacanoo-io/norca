// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"testing"

	"github.com/bacanoo-io/norca/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
)

func TestSystem_GarbageCollect(t *testing.T) {
	testutil.Parallel(t)

	c, s := makeClient(t, nil, nil)
	defer s.Stop()
	e := c.System()
	err := e.GarbageCollect()
	must.NoError(t, err)
}
