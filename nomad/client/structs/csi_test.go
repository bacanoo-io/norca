// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/csi"
	"github.com/shoenig/test/must"
)

func TestCSIVolumeMountOptions_ToCSIMountOptions(t *testing.T) {
	ci.Parallel(t)

	var nilOpts *CSIVolumeMountOptions
	must.Nil(t, nilOpts.ToCSIMountOptions())

	opts := &CSIVolumeMountOptions{
		Filesystem: "ext4",
		MountFlags: []string{"ro", "noatime"},
	}
	must.Eq(t, &structs.CSIMountOptions{
		FSType:     "ext4",
		MountFlags: []string{"ro", "noatime"},
	}, opts.ToCSIMountOptions())

	must.Eq(t, &structs.CSIMountOptions{}, (&CSIVolumeMountOptions{}).ToCSIMountOptions())
}

func TestCSIControllerQuery_SetControllerNodeID(t *testing.T) {
	ci.Parallel(t)

	// every controller request embeds CSIControllerQuery so that the server
	// can target it at a specific controller node
	reqs := []CSIControllerRequest{
		&ClientCSIControllerValidateVolumeRequest{},
		&ClientCSIControllerAttachVolumeRequest{},
		&ClientCSIControllerDetachVolumeRequest{},
		&ClientCSIControllerCreateVolumeRequest{},
		&ClientCSIControllerExpandVolumeRequest{},
		&ClientCSIControllerDeleteVolumeRequest{},
		&ClientCSIControllerListVolumesRequest{},
		&ClientCSIControllerCreateSnapshotRequest{},
		&ClientCSIControllerDeleteSnapshotRequest{},
		&ClientCSIControllerListSnapshotsRequest{},
	}
	for _, req := range reqs {
		req.SetControllerNodeID("node-1")
	}

	q := &CSIControllerQuery{PluginID: "plug"}
	q.SetControllerNodeID("node-2")
	must.Eq(t, CSIControllerQuery{ControllerNodeID: "node-2", PluginID: "plug"}, *q)

	attach := reqs[1].(*ClientCSIControllerAttachVolumeRequest)
	must.Eq(t, "node-1", attach.ControllerNodeID)
}

func TestClientCSIControllerValidateVolumeRequest_ToCSIRequest(t *testing.T) {
	ci.Parallel(t)

	t.Run("nil request", func(t *testing.T) {
		var req *ClientCSIControllerValidateVolumeRequest
		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, &csi.ControllerValidateVolumeRequest{}, creq)
	})

	t.Run("capabilities", func(t *testing.T) {
		mountOpts := &structs.CSIMountOptions{FSType: "xfs", MountFlags: []string{"noatime"}}
		req := &ClientCSIControllerValidateVolumeRequest{
			VolumeID: "ext-vol",
			VolumeCapabilities: []*structs.CSIVolumeCapability{
				{
					AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
					AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
				},
				{
					AttachmentMode: structs.CSIVolumeAttachmentModeBlockDevice,
					AccessMode:     structs.CSIVolumeAccessModeMultiNodeReader,
				},
			},
			MountOptions: mountOpts,
			Secrets:      structs.CSISecrets{"key": "secret"},
			Parameters:   map[string]string{"param": "1"},
			Context:      map[string]string{"ctx": "2"},
		}

		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, &csi.ControllerValidateVolumeRequest{
			ExternalID: "ext-vol",
			Secrets:    structs.CSISecrets{"key": "secret"},
			Capabilities: []*csi.VolumeCapability{
				{
					AccessType:  csi.VolumeAccessTypeMount,
					AccessMode:  csi.VolumeAccessModeSingleNodeWriter,
					MountVolume: mountOpts,
				},
				{
					AccessType:  csi.VolumeAccessTypeBlock,
					AccessMode:  csi.VolumeAccessModeMultiNodeReaderOnly,
					MountVolume: mountOpts,
				},
			},
			Parameters: map[string]string{"param": "1"},
			Context:    map[string]string{"ctx": "2"},
		}, creq)
	})

	t.Run("no capabilities", func(t *testing.T) {
		req := &ClientCSIControllerValidateVolumeRequest{VolumeID: "ext-vol"}
		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.NotNil(t, creq.Capabilities)
		must.SliceEmpty(t, creq.Capabilities)
	})

	t.Run("invalid capability", func(t *testing.T) {
		req := &ClientCSIControllerValidateVolumeRequest{
			VolumeID: "ext-vol",
			VolumeCapabilities: []*structs.CSIVolumeCapability{{
				AttachmentMode: "bogus",
				AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
			}},
		}
		creq, err := req.ToCSIRequest()
		must.ErrorContains(t, err, "unknown volume attachment mode")
		must.Nil(t, creq)
	})
}

func TestClientCSIControllerAttachVolumeRequest_ToCSIRequest(t *testing.T) {
	ci.Parallel(t)

	t.Run("nil request", func(t *testing.T) {
		var req *ClientCSIControllerAttachVolumeRequest
		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, &csi.ControllerPublishVolumeRequest{}, creq)
	})

	t.Run("full request", func(t *testing.T) {
		req := &ClientCSIControllerAttachVolumeRequest{
			VolumeID:        "ext-vol",
			ClientCSINodeID: "csi-node",
			AttachmentMode:  structs.CSIVolumeAttachmentModeFilesystem,
			AccessMode:      structs.CSIVolumeAccessModeMultiNodeMultiWriter,
			MountOptions: &CSIVolumeMountOptions{
				Filesystem: "ext4",
				MountFlags: []string{"sync"},
			},
			ReadOnly:      true,
			Secrets:       structs.CSISecrets{"key": "secret"},
			VolumeContext: map[string]string{"ctx": "1"},
		}

		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, &csi.ControllerPublishVolumeRequest{
			ExternalID: "ext-vol",
			NodeID:     "csi-node",
			ReadOnly:   true,
			VolumeCapability: &csi.VolumeCapability{
				AccessType: csi.VolumeAccessTypeMount,
				AccessMode: csi.VolumeAccessModeMultiNodeMultiWriter,
				MountVolume: &structs.CSIMountOptions{
					FSType:     "ext4",
					MountFlags: []string{"sync"},
				},
			},
			Secrets:       structs.CSISecrets{"key": "secret"},
			VolumeContext: map[string]string{"ctx": "1"},
		}, creq)
	})

	t.Run("no mount options", func(t *testing.T) {
		req := &ClientCSIControllerAttachVolumeRequest{
			VolumeID:        "ext-vol",
			ClientCSINodeID: "csi-node",
			AttachmentMode:  structs.CSIVolumeAttachmentModeBlockDevice,
			AccessMode:      structs.CSIVolumeAccessModeSingleNodeReader,
		}
		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, csi.VolumeAccessTypeBlock, creq.VolumeCapability.AccessType)
		must.Eq(t, csi.VolumeAccessModeSingleNodeReaderOnly, creq.VolumeCapability.AccessMode)
		must.Nil(t, creq.VolumeCapability.MountVolume)
	})

	t.Run("invalid access mode", func(t *testing.T) {
		req := &ClientCSIControllerAttachVolumeRequest{
			VolumeID:       "ext-vol",
			AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
			AccessMode:     "bogus",
		}
		creq, err := req.ToCSIRequest()
		must.ErrorContains(t, err, "unknown volume access mode")
		must.Nil(t, creq)
	})
}

func TestClientCSIControllerDetachVolumeRequest_ToCSIRequest(t *testing.T) {
	ci.Parallel(t)

	var nilReq *ClientCSIControllerDetachVolumeRequest
	must.Eq(t, &csi.ControllerUnpublishVolumeRequest{}, nilReq.ToCSIRequest())

	req := &ClientCSIControllerDetachVolumeRequest{
		VolumeID:        "ext-vol",
		ClientCSINodeID: "csi-node",
		Secrets:         structs.CSISecrets{"key": "secret"},
	}
	must.Eq(t, &csi.ControllerUnpublishVolumeRequest{
		ExternalID: "ext-vol",
		NodeID:     "csi-node",
	}, req.ToCSIRequest())
}

func TestClientCSIControllerCreateVolumeRequest_ToCSIRequest(t *testing.T) {
	ci.Parallel(t)

	mountOpts := &structs.CSIMountOptions{FSType: "ext4"}

	baseRequest := func() *ClientCSIControllerCreateVolumeRequest {
		return &ClientCSIControllerCreateVolumeRequest{
			Name: "vol",
			VolumeCapabilities: []*structs.CSIVolumeCapability{{
				AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
				AccessMode:     structs.CSIVolumeAccessModeMultiNodeSingleWriter,
			}},
			MountOptions: mountOpts,
			Parameters:   map[string]string{"param": "1"},
			Secrets:      structs.CSISecrets{"key": "secret"},
			SnapshotID:   "snap-1",
			CloneID:      "clone-1",
		}
	}

	t.Run("full request", func(t *testing.T) {
		req := baseRequest()
		req.CapacityMin = 10
		req.CapacityMax = 20
		req.RequestedTopologies = &structs.CSITopologyRequest{
			Required: []*structs.CSITopology{
				{Segments: map[string]string{"rack": "r1"}},
			},
			Preferred: []*structs.CSITopology{
				{Segments: map[string]string{"zone": "z1"}},
				{Segments: map[string]string{"zone": "z2"}},
			},
		}

		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, &csi.ControllerCreateVolumeRequest{
			Name: "vol",
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: 10,
				LimitBytes:    20,
			},
			VolumeCapabilities: []*csi.VolumeCapability{{
				AccessType:  csi.VolumeAccessTypeMount,
				AccessMode:  csi.VolumeAccessModeMultiNodeSingleWriter,
				MountVolume: mountOpts,
			}},
			Parameters: map[string]string{"param": "1"},
			Secrets:    structs.CSISecrets{"key": "secret"},
			ContentSource: &csi.VolumeContentSource{
				SnapshotID: "snap-1",
				CloneID:    "clone-1",
			},
			AccessibilityRequirements: &csi.TopologyRequirement{
				Requisite: []*csi.Topology{
					{Segments: map[string]string{"rack": "r1"}},
				},
				Preferred: []*csi.Topology{
					{Segments: map[string]string{"zone": "z1"}},
					{Segments: map[string]string{"zone": "z2"}},
				},
			},
		}, creq)
	})

	t.Run("capacity unset", func(t *testing.T) {
		creq, err := baseRequest().ToCSIRequest()
		must.NoError(t, err)
		must.Nil(t, creq.CapacityRange)
	})

	t.Run("only minimum capacity", func(t *testing.T) {
		req := baseRequest()
		req.CapacityMin = 5
		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, &csi.CapacityRange{RequiredBytes: 5}, creq.CapacityRange)
	})

	t.Run("only maximum capacity", func(t *testing.T) {
		req := baseRequest()
		req.CapacityMax = 7
		creq, err := req.ToCSIRequest()
		must.NoError(t, err)
		must.Eq(t, &csi.CapacityRange{LimitBytes: 7}, creq.CapacityRange)
	})

	t.Run("no topologies", func(t *testing.T) {
		creq, err := baseRequest().ToCSIRequest()
		must.NoError(t, err)
		must.NotNil(t, creq.AccessibilityRequirements)
		must.SliceEmpty(t, creq.AccessibilityRequirements.Requisite)
		must.SliceEmpty(t, creq.AccessibilityRequirements.Preferred)
	})

	t.Run("invalid capability", func(t *testing.T) {
		req := baseRequest()
		req.VolumeCapabilities = append(req.VolumeCapabilities, &structs.CSIVolumeCapability{
			AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
			AccessMode:     structs.CSIVolumeAccessModeUnknown,
		})
		creq, err := req.ToCSIRequest()
		must.ErrorContains(t, err, "unknown volume access mode")
		must.Nil(t, creq)
	})
}

func TestClientCSIControllerExpandVolumeRequest_ToCSIRequest(t *testing.T) {
	ci.Parallel(t)

	capability := &csi.VolumeCapability{
		AccessType: csi.VolumeAccessTypeMount,
		AccessMode: csi.VolumeAccessModeSingleNodeWriter,
	}

	t.Run("with capacity range", func(t *testing.T) {
		req := &ClientCSIControllerExpandVolumeRequest{
			ExternalVolumeID: "ext-vol",
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: 100,
				LimitBytes:    200,
			},
			Secrets:          structs.CSISecrets{"key": "secret"},
			VolumeCapability: capability,
		}
		must.Eq(t, &csi.ControllerExpandVolumeRequest{
			ExternalVolumeID: "ext-vol",
			RequiredBytes:    100,
			LimitBytes:       200,
			Capability:       capability,
			Secrets:          structs.CSISecrets{"key": "secret"},
		}, req.ToCSIRequest())
	})

	t.Run("without capacity range", func(t *testing.T) {
		req := &ClientCSIControllerExpandVolumeRequest{
			ExternalVolumeID: "ext-vol",
		}
		must.Eq(t, &csi.ControllerExpandVolumeRequest{
			ExternalVolumeID: "ext-vol",
		}, req.ToCSIRequest())
	})
}

func TestClientCSIControllerVolumeRequests_ToCSIRequest(t *testing.T) {
	ci.Parallel(t)

	secrets := structs.CSISecrets{"key": "secret"}

	del := &ClientCSIControllerDeleteVolumeRequest{
		ExternalVolumeID: "ext-vol",
		Secrets:          secrets,
	}
	must.Eq(t, &csi.ControllerDeleteVolumeRequest{
		ExternalVolumeID: "ext-vol",
		Secrets:          secrets,
	}, del.ToCSIRequest())

	list := &ClientCSIControllerListVolumesRequest{
		MaxEntries:    10,
		StartingToken: "token",
	}
	must.Eq(t, &csi.ControllerListVolumesRequest{
		MaxEntries:    10,
		StartingToken: "token",
	}, list.ToCSIRequest())
}

func TestClientCSIControllerSnapshotRequests_ToCSIRequest(t *testing.T) {
	ci.Parallel(t)

	secrets := structs.CSISecrets{"key": "secret"}

	create := &ClientCSIControllerCreateSnapshotRequest{
		ExternalSourceVolumeID: "ext-vol",
		Name:                   "snap",
		Secrets:                secrets,
		Parameters:             map[string]string{"param": "1"},
	}
	creq, err := create.ToCSIRequest()
	must.NoError(t, err)
	must.Eq(t, &csi.ControllerCreateSnapshotRequest{
		VolumeID:   "ext-vol",
		Name:       "snap",
		Secrets:    secrets,
		Parameters: map[string]string{"param": "1"},
	}, creq)

	del := &ClientCSIControllerDeleteSnapshotRequest{
		ID:      "snap-1",
		Secrets: secrets,
	}
	must.Eq(t, &csi.ControllerDeleteSnapshotRequest{
		SnapshotID: "snap-1",
		Secrets:    secrets,
	}, del.ToCSIRequest())

	list := &ClientCSIControllerListSnapshotsRequest{
		MaxEntries:    5,
		StartingToken: "token",
		Secrets:       secrets,
	}
	must.Eq(t, &csi.ControllerListSnapshotsRequest{
		MaxEntries:    5,
		StartingToken: "token",
		Secrets:       secrets,
	}, list.ToCSIRequest())
}

func TestClientCSINodeExpandVolumeRequest_Validate(t *testing.T) {
	ci.Parallel(t)

	validRequest := func() *ClientCSINodeExpandVolumeRequest {
		return &ClientCSINodeExpandVolumeRequest{
			PluginID:   "plug",
			VolumeID:   "vol",
			ExternalID: "ext-vol",
			Capacity:   &csi.CapacityRange{RequiredBytes: 10},
			Claim:      &structs.CSIVolumeClaim{AllocationID: "alloc"},
		}
	}

	cases := []struct {
		name      string
		modify    func(*ClientCSINodeExpandVolumeRequest)
		expectErr []string
	}{
		{
			name:   "valid",
			modify: func(*ClientCSINodeExpandVolumeRequest) {},
		},
		{
			name:      "missing plugin ID",
			modify:    func(r *ClientCSINodeExpandVolumeRequest) { r.PluginID = "" },
			expectErr: []string{"PluginID is required"},
		},
		{
			name:      "missing volume ID",
			modify:    func(r *ClientCSINodeExpandVolumeRequest) { r.VolumeID = "" },
			expectErr: []string{"VolumeID is required"},
		},
		{
			name:      "missing external ID",
			modify:    func(r *ClientCSINodeExpandVolumeRequest) { r.ExternalID = "" },
			expectErr: []string{"ExternalID is required"},
		},
		{
			name:      "missing claim",
			modify:    func(r *ClientCSINodeExpandVolumeRequest) { r.Claim = nil },
			expectErr: []string{"Claim is required"},
		},
		{
			name: "missing claim allocation ID",
			modify: func(r *ClientCSINodeExpandVolumeRequest) {
				r.Claim = &structs.CSIVolumeClaim{}
			},
			expectErr: []string{"Claim.AllocationID is required"},
		},
		{
			name: "everything missing",
			modify: func(r *ClientCSINodeExpandVolumeRequest) {
				*r = ClientCSINodeExpandVolumeRequest{}
			},
			expectErr: []string{
				"PluginID is required",
				"VolumeID is required",
				"ExternalID is required",
				"Claim is required",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.modify(req)
			err := req.Validate()
			if len(tc.expectErr) == 0 {
				must.NoError(t, err)
				return
			}
			must.Error(t, err)
			for _, msg := range tc.expectErr {
				must.ErrorContains(t, err, msg)
			}
			if tc.name != "missing claim allocation ID" {
				must.StrNotContains(t, err.Error(), "Claim.AllocationID")
			}
		})
	}
}
