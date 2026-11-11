// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csi

import (
	"testing"

	csipbv1 "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// TestVolumeCapability_SingleNodeModesAndMountGroup checks the CSI
// representation of the single-node modes of CSI 1.5 and of mount_group, and
// the capabilities that allow them (clean room: CSI spec v1.7.0).
func TestVolumeCapability_SingleNodeModesAndMountGroup(t *testing.T) {
	ci.Parallel(t)

	for mode, want := range map[structs.CSIVolumeAccessMode]csipbv1.VolumeCapability_AccessMode_Mode{
		structs.CSIVolumeAccessModeSingleNodeSingleWriter: csipbv1.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		structs.CSIVolumeAccessModeSingleNodeMultiWriter:  csipbv1.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	} {
		c, err := VolumeCapabilityFromStructs(structs.CSIVolumeAttachmentModeFilesystem, mode,
			&structs.CSIMountOptions{FSType: "ext4", MountGroup: "1000"})
		must.NoError(t, err)
		pb := c.ToCSIRepresentation()
		must.Eq(t, want, pb.GetAccessMode().GetMode())
		must.Eq(t, "1000", pb.GetMount().GetVolumeMountGroup())
		must.Eq(t, "ext4", pb.GetMount().GetFsType())
	}

	rpc := func(t csipbv1.NodeServiceCapability_RPC_Type) *csipbv1.NodeServiceCapability {
		return &csipbv1.NodeServiceCapability{Type: &csipbv1.NodeServiceCapability_Rpc{Rpc: &csipbv1.NodeServiceCapability_RPC{Type: t}}}
	}
	ns := NewNodeCapabilitySet(&csipbv1.NodeGetCapabilitiesResponse{Capabilities: []*csipbv1.NodeServiceCapability{
		rpc(csipbv1.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER),
		rpc(csipbv1.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP),
	}})
	must.True(t, ns.HasSingleNodeMultiWriter)
	must.True(t, ns.HasVolumeMountGroup)

	cs := NewControllerCapabilitySet(&csipbv1.ControllerGetCapabilitiesResponse{Capabilities: []*csipbv1.ControllerServiceCapability{{
		Type: &csipbv1.ControllerServiceCapability_Rpc{Rpc: &csipbv1.ControllerServiceCapability_RPC{
			Type: csipbv1.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER}},
	}}})
	must.True(t, cs.HasSingleNodeMultiWriter)
}
