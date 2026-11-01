// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package volumewatcher

import (
	"testing"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/helper/testlog"
	"github.com/bacanoo-io/norca/nomad/nomad/mock"
	"github.com/bacanoo-io/norca/nomad/nomad/state"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/stretchr/testify/require"
)

func TestVolumeWatch_Reap(t *testing.T) {
	ci.Parallel(t)
	require := require.New(t)

	srv := &MockRPCServer{
		state: state.TestStateStore(t),
	}

	plugin := mock.CSIPlugin()
	node := testNode(plugin, srv.State())
	alloc := mock.Alloc()
	alloc.NodeID = node.ID
	alloc.ClientStatus = structs.AllocClientStatusComplete
	vol := testVolume(plugin, alloc, node.ID)
	vol.PastClaims = vol.ReadClaims

	w := &volumeWatcher{
		v:      vol,
		rpc:    srv,
		state:  srv.State(),
		logger: testlog.HCLogger(t),
	}

	vol, _ = srv.State().CSIVolumeDenormalize(nil, vol.Copy())
	err := w.volumeReapImpl(vol)
	require.NoError(err)

	// past claim from a previous pass
	vol.PastClaims = map[string]*structs.CSIVolumeClaim{
		alloc.ID: {
			NodeID: node.ID,
			Mode:   structs.CSIVolumeClaimRead,
			State:  structs.CSIVolumeClaimStateNodeDetached,
		},
	}
	vol, _ = srv.State().CSIVolumeDenormalize(nil, vol.Copy())
	err = w.volumeReapImpl(vol)
	require.NoError(err)
	require.Len(vol.PastClaims, 1)

	// claim emitted by a GC event
	vol.PastClaims = map[string]*structs.CSIVolumeClaim{
		"": {
			NodeID: node.ID,
			Mode:   structs.CSIVolumeClaimGC,
		},
	}
	vol, _ = srv.State().CSIVolumeDenormalize(nil, vol.Copy())
	err = w.volumeReapImpl(vol)
	require.NoError(err)
	require.Len(vol.PastClaims, 2) // alloc claim + GC claim

	// release claims of a previously GC'd allocation
	vol.ReadAllocs[alloc.ID] = nil
	vol.PastClaims = map[string]*structs.CSIVolumeClaim{
		"": {
			NodeID: node.ID,
			Mode:   structs.CSIVolumeClaimRead,
		},
	}
	vol, _ = srv.State().CSIVolumeDenormalize(nil, vol.Copy())
	err = w.volumeReapImpl(vol)
	require.NoError(err)
	require.Len(vol.PastClaims, 2) // alloc claim + GC claim
}

func TestVolumeReapBadState(t *testing.T) {
	ci.Parallel(t)

	store := state.TestStateStore(t)
	err := state.TestBadCSIState(t, store)
	require.NoError(t, err)
	srv := &MockRPCServer{
		state: store,
	}

	vol, err := srv.state.CSIVolumeByID(nil,
		structs.DefaultNamespace, "csi-volume-nfs0")
	require.NoError(t, err)
	srv.state.CSIVolumeDenormalize(nil, vol)

	w := &volumeWatcher{
		v:      vol,
		rpc:    srv,
		state:  srv.State(),
		logger: testlog.HCLogger(t),
	}

	err = w.volumeReapImpl(vol)
	require.NoError(t, err)
	require.Equal(t, int64(2), srv.countCSIUnpublish.Load())
}
