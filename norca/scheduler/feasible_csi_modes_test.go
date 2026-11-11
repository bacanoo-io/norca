// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// TestCSIVolumeChecker_SingleNodeModesAndMountGroup checks the placement of
// the single-node modes of CSI 1.5 and of mount_group: only on nodes whose
// plugin has the capability, and a single-node-multi-writer volume only on
// the node of its claims.
func TestCSIVolumeChecker_SingleNodeModesAndMountGroup(t *testing.T) {
	ci.Parallel(t)
	state, ctx := testContext(t)

	nodeWith := func(snmw, group bool) *structs.Node {
		n := mock.Node()
		n.CSINodePlugins = map[string]*structs.CSIInfo{
			"foo": {PluginID: "foo", Healthy: true, NodeInfo: &structs.CSINodeInfo{
				MaxVolumes: 10, SupportsSingleNodeMultiWriter: snmw, SupportsVolumeMountGroup: group,
			}},
		}
		return n
	}
	capable, capable2, plain := nodeWith(true, true), nodeWith(true, false), nodeWith(false, false)
	index := uint64(999)
	for _, n := range []*structs.Node{capable, capable2, plain} {
		must.NoError(t, state.UpsertNode(structs.MsgTypeTestSetup, index, n))
		index++
	}

	vol := structs.NewCSIVolume("vol", index)
	vol.PluginID = "foo"
	vol.Namespace = structs.DefaultNamespace
	vol.Schedulable = true
	vol.RequestedCapabilities = []*structs.CSIVolumeCapability{{
		AccessMode: structs.CSIVolumeAccessModeSingleNodeMultiWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
	}}
	must.NoError(t, state.UpsertCSIVolume(index, []*structs.CSIVolume{vol}))
	index++

	checker := NewCSIVolumeChecker(ctx)
	checker.SetNamespace(structs.DefaultNamespace)
	request := func(group string) map[string]*structs.VolumeRequest {
		r := &structs.VolumeRequest{
			Name: "data", Type: structs.VolumeTypeCSI, Source: "vol",
			AccessMode: structs.CSIVolumeAccessModeSingleNodeMultiWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		}
		if group != "" {
			r.MountOptions = &structs.CSIMountOptions{MountGroup: group}
		}
		return map[string]*structs.VolumeRequest{"data": r}
	}

	checker.SetVolumes("web[0]", request(""))
	must.True(t, checker.Feasible(capable))
	must.True(t, checker.Feasible(capable2))
	must.False(t, checker.Feasible(plain), must.Sprint("no SINGLE_NODE_MULTI_WRITER"))

	checker.SetVolumes("web[0]", request("1000"))
	must.True(t, checker.Feasible(capable))
	must.False(t, checker.Feasible(capable2), must.Sprint("no VOLUME_MOUNT_GROUP"))

	// Claimed on the first node: only that node fits.
	alloc := mock.Alloc()
	alloc.NodeID = capable.ID
	must.NoError(t, state.UpsertAllocs(structs.MsgTypeTestSetup, index, []*structs.Allocation{alloc}))
	index++
	must.NoError(t, state.CSIVolumeClaim(index, structs.DefaultNamespace, "vol", &structs.CSIVolumeClaim{
		AllocationID: alloc.ID, NodeID: capable.ID, Mode: structs.CSIVolumeClaimWrite, State: structs.CSIVolumeClaimStateTaken,
		AccessMode: structs.CSIVolumeAccessModeSingleNodeMultiWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
	}))

	checker.SetVolumes("web[1]", request(""))
	must.True(t, checker.Feasible(capable))
	must.False(t, checker.Feasible(capable2), must.Sprint("claimed on another node"))
}
