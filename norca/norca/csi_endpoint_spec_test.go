// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/csi"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// specPlugin returns a plugin with one controller that has the controller
// capabilities set by fn.
func specPlugin(fn func(*structs.CSIControllerInfo)) *structs.CSIPlugin {
	info := &structs.CSIControllerInfo{SupportsCreateDelete: true, SupportsAttachDetach: true}
	if fn != nil {
		fn(info)
	}
	return &structs.CSIPlugin{
		ID: "minnie",
		Controllers: map[string]*structs.CSIInfo{
			"node-1": {PluginID: "minnie", Healthy: true, ControllerInfo: info},
		},
	}
}

func TestCSI_ControllerPublishReadOnly(t *testing.T) {
	ci.Parallel(t)

	without := specPlugin(nil)
	with := specPlugin(func(c *structs.CSIControllerInfo) { c.SupportsReadOnlyAttach = true })

	// The spec allows readonly only with PUBLISH_READONLY.
	must.False(t, controllerPublishReadOnly(structs.CSIVolumeClaimRead, without))
	must.True(t, controllerPublishReadOnly(structs.CSIVolumeClaimRead, with))
	must.False(t, controllerPublishReadOnly(structs.CSIVolumeClaimWrite, with))
}

func TestCSI_CheckCreateSource(t *testing.T) {
	ci.Parallel(t)

	plain := specPlugin(nil)
	clone := specPlugin(func(c *structs.CSIControllerInfo) { c.SupportsClone = true })
	snap := specPlugin(func(c *structs.CSIControllerInfo) { c.SupportsCreateDeleteSnapshot = true })

	must.NoError(t, checkCreateSource(&structs.CSIVolume{ID: "v"}, plain))

	fromClone := &structs.CSIVolume{ID: "v", CloneID: "src"}
	must.ErrorContains(t, checkCreateSource(fromClone, plain), "does not support cloning volumes")
	must.NoError(t, checkCreateSource(fromClone, clone))

	fromSnap := &structs.CSIVolume{ID: "v", SnapshotID: "snap-1"}
	must.ErrorContains(t, checkCreateSource(fromSnap, plain), "from snapshots")
	must.NoError(t, checkCreateSource(fromSnap, snap))
}

// TestCSI_NodeExpandOnlyWithCapability checks that NodeExpandVolume goes only
// to nodes whose node plugin has EXPAND_VOLUME.
func TestCSI_NodeExpandOnlyWithCapability(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)

	nodeWith := func(expand bool) *structs.Node {
		n := mock.Node()
		n.CSINodePlugins = map[string]*structs.CSIInfo{
			"minnie": {PluginID: "minnie", Healthy: true,
				NodeInfo: &structs.CSINodeInfo{ID: n.ID, MaxVolumes: 4, SupportsExpand: expand}},
		}
		return n
	}
	can, cannot := nodeWith(true), nodeWith(false)
	store := srv.fsm.State()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1000, can))
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1001, cannot))

	endpoint := NewCSIVolumeEndpoint(srv, nil)
	plugin := &structs.CSIPlugin{ID: "minnie"}
	must.True(t, endpoint.nodePluginCanExpand(can.ID, plugin))
	must.False(t, endpoint.nodePluginCanExpand(cannot.ID, plugin))

	// A node unknown to the state falls back to the plugin's capability.
	must.False(t, endpoint.nodePluginCanExpand("no-such-node", plugin))

	// Only the claim on the capable node is sent: it fails, as the mock node
	// has no client connection; the other one is skipped without an error.
	vol := &structs.CSIVolume{
		ID: "vol", ExternalID: "ext",
		WriteClaims: map[string]*structs.CSIVolumeClaim{
			"a1": {AllocationID: "a1", NodeID: cannot.ID, Mode: structs.CSIVolumeClaimWrite},
		},
	}
	must.NoError(t, endpoint.nodeExpandVolume(vol, plugin, &csi.CapacityRange{RequiredBytes: 10}))

	vol.WriteClaims["a2"] = &structs.CSIVolumeClaim{AllocationID: "a2", NodeID: can.ID, Mode: structs.CSIVolumeClaimWrite}
	must.Error(t, endpoint.nodeExpandVolume(vol, plugin, &csi.CapacityRange{RequiredBytes: 10}))
}
