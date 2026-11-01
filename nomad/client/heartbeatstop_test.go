// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/client/config"
	"github.com/bacanoo-io/norca/nomad/helper/uuid"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/bacanoo-io/norca/nomad/testutil"
	"github.com/stretchr/testify/require"
)

func TestHeartbeatStop_allocHook(t *testing.T) {
	ci.Parallel(t)

	server, _, cleanupS1 := testServer(t, nil)
	defer cleanupS1()
	testutil.WaitForLeader(t, server.RPC)

	client, cleanupC1 := TestClient(t, func(c *config.Config) {
		c.RPCHandler = server
	})
	defer cleanupC1()

	// an allocation, with a tiny lease
	d := 1 * time.Microsecond
	alloc := &structs.Allocation{
		ID:        uuid.Generate(),
		TaskGroup: "foo",
		Job: &structs.Job{
			TaskGroups: []*structs.TaskGroup{
				{
					Name:                      "foo",
					StopAfterClientDisconnect: &d,
				},
			},
		},
		Resources: &structs.Resources{
			CPU:      100,
			MemoryMB: 100,
			DiskMB:   0,
		},
	}

	// alloc added to heartbeatStop.allocs
	err := client.addAlloc(alloc, "")
	require.NoError(t, err)
	testutil.WaitForResult(func() (bool, error) {
		_, ok := client.heartbeatStop.allocInterval[alloc.ID]
		return ok, nil
	}, func(err error) {
		require.NoError(t, err)
	})

	// the tiny lease causes the watch loop to destroy it
	testutil.WaitForResult(func() (bool, error) {
		_, ok := client.heartbeatStop.allocInterval[alloc.ID]
		return !ok, nil
	}, func(err error) {
		require.NoError(t, err)
	})

	require.Empty(t, client.allocs[alloc.ID])
}
