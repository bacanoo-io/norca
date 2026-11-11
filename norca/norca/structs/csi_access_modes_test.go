// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// TestCSIVolume_SingleNodeModes checks the claim rules of the single-node
// modes of CSI 1.5 (clean room): single-node-single-writer allows one
// writer, single-node-multi-writer any number of claims on the node of the
// first one and none on another node.
func TestCSIVolume_SingleNodeModes(t *testing.T) {
	ci.Parallel(t)

	alloc := func(id string) *Allocation { return &Allocation{ID: id} }
	claim := func(id, node string, mode CSIVolumeClaimMode, am CSIVolumeAccessMode) *CSIVolumeClaim {
		return &CSIVolumeClaim{
			AllocationID: id, NodeID: node, Mode: mode, State: CSIVolumeClaimStateTaken,
			AccessMode: am, AttachmentMode: CSIVolumeAttachmentModeFilesystem,
		}
	}

	t.Run("single-node-multi-writer", func(t *testing.T) {
		v := NewCSIVolume("vol", 1)
		v.Schedulable = true
		m := CSIVolumeAccessModeSingleNodeMultiWriter
		v.RequestedCapabilities = []*CSIVolumeCapability{{AccessMode: m, AttachmentMode: CSIVolumeAttachmentModeFilesystem}}

		must.NoError(t, v.Claim(claim("a1", "n1", CSIVolumeClaimWrite, m), alloc("a1")))
		must.Eq(t, m, v.AccessMode)
		must.NoError(t, v.Claim(claim("a2", "n1", CSIVolumeClaimWrite, m), alloc("a2")))
		must.NoError(t, v.Claim(claim("a3", "n1", CSIVolumeClaimRead, m), alloc("a3")))
		must.MapLen(t, 2, v.WriteClaims)

		must.ErrorIs(t, v.Claim(claim("a4", "n2", CSIVolumeClaimWrite, m), alloc("a4")), ErrCSIVolumeMaxClaims)
		must.ErrorIs(t, v.Claim(claim("a5", "n2", CSIVolumeClaimRead, m), alloc("a5")), ErrCSIVolumeMaxClaims)
		must.True(t, v.ClaimedOnOtherNode("n2"))
		must.False(t, v.ClaimedOnOtherNode("n1"))
		must.True(t, v.HasFreeWriteClaimsOnNode("n1"))
		must.False(t, v.HasFreeWriteClaimsOnNode("n2"))
	})

	t.Run("single-node-single-writer", func(t *testing.T) {
		v := NewCSIVolume("vol", 1)
		v.Schedulable = true
		m := CSIVolumeAccessModeSingleNodeSingleWriter
		v.RequestedCapabilities = []*CSIVolumeCapability{{AccessMode: m, AttachmentMode: CSIVolumeAttachmentModeFilesystem}}

		must.NoError(t, v.Claim(claim("a1", "n1", CSIVolumeClaimWrite, m), alloc("a1")))
		must.ErrorIs(t, v.Claim(claim("a2", "n1", CSIVolumeClaimWrite, m), alloc("a2")), ErrCSIVolumeMaxClaims)
		must.ErrorIs(t, v.Claim(claim("a3", "n1", CSIVolumeClaimRead, m), alloc("a3")), ErrCSIVolumeMaxClaims)
		must.False(t, v.ClaimedOnOtherNode("n2"), must.Sprint("only the multi-writer mode is bound to a node this way"))
	})

	must.True(t, IsSingleNodeMultiWriterMode(CSIVolumeAccessModeSingleNodeSingleWriter))
	must.True(t, IsSingleNodeMultiWriterMode(CSIVolumeAccessModeSingleNodeMultiWriter))
	must.False(t, IsSingleNodeMultiWriterMode(CSIVolumeAccessModeSingleNodeWriter))
}

// TestVolumeRequest_SingleNodeModesAndMountGroup checks the job validation of
// the new modes and of mount_group.
func TestVolumeRequest_SingleNodeModesAndMountGroup(t *testing.T) {
	ci.Parallel(t)

	req := func(m CSIVolumeAccessMode, a CSIVolumeAttachmentMode, group string) *VolumeRequest {
		r := &VolumeRequest{Type: VolumeTypeCSI, Source: "vol", AccessMode: m, AttachmentMode: a}
		if group != "" {
			r.MountOptions = &CSIMountOptions{MountGroup: group}
		}
		return r
	}
	fs := CSIVolumeAttachmentModeFilesystem
	must.ErrorContains(t, req(CSIVolumeAccessModeSingleNodeSingleWriter, fs, "").Validate(JobTypeService, 2, 0), "only one reader or writer")
	must.NoError(t, req(CSIVolumeAccessModeSingleNodeSingleWriter, fs, "").Validate(JobTypeService, 1, 0))
	must.NoError(t, req(CSIVolumeAccessModeSingleNodeMultiWriter, fs, "").Validate(JobTypeService, 3, 0))
	must.NoError(t, req(CSIVolumeAccessModeSingleNodeMultiWriter, fs, "1000").Validate(JobTypeService, 1, 0))
	must.ErrorContains(t, req(CSIVolumeAccessModeSingleNodeMultiWriter, CSIVolumeAttachmentModeBlockDevice, "1000").Validate(JobTypeService, 1, 0), "mount_group")

	v := &CSIVolume{ID: "v", PluginID: "p", Namespace: "default",
		RequestedCapabilities: []*CSIVolumeCapability{{AccessMode: CSIVolumeAccessModeSingleNodeMultiWriter, AttachmentMode: fs}},
		MountOptions:          &CSIMountOptions{MountGroup: "1000"}}
	must.ErrorContains(t, v.Validate(), "mount_group belongs in the volume block of the job")

	o := &CSIMountOptions{FSType: "ext4"}
	o.Merge(&CSIMountOptions{MountGroup: "1000"})
	must.Eq(t, "1000", o.MountGroup)
	must.False(t, o.Equal(&CSIMountOptions{FSType: "ext4"}))
}
