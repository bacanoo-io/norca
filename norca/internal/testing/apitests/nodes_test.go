// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package apitests

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/stretchr/testify/require"
)

func TestNodes_GC(t *testing.T) {
	ci.Parallel(t)
	require := require.New(t)
	c, s := makeClient(t, nil, nil)
	defer s.Stop()
	nodes := c.Nodes()

	err := nodes.GC(uuid.Generate(), nil)
	require.NotNil(err)
	require.True(structs.IsErrUnknownNode(err))
}

func TestNodes_GcAlloc(t *testing.T) {
	ci.Parallel(t)
	require := require.New(t)
	c, s := makeClient(t, nil, nil)
	defer s.Stop()
	nodes := c.Nodes()

	err := nodes.GcAlloc(uuid.Generate(), nil)
	require.NotNil(err)
	require.True(structs.IsErrUnknownAllocation(err))
}
