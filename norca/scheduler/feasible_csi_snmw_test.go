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

// The allocations of a group with a count above one that use a
// single-node-multi-writer volume are placed on one node: the volume has no
// claim yet when they are placed, so the placements of the plan pin it
// (review finding SCH-7: they went to two nodes, and the second claim was
// retried on the wrong node for up to a day).
func TestServiceSched_SingleNodeMultiWriterVolumeOneNode(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	var nodes []*structs.Node
	for range 3 {
		n := mock.Node()
		n.CSINodePlugins = map[string]*structs.CSIInfo{
			"foo": {PluginID: "foo", Healthy: true, NodeInfo: &structs.CSINodeInfo{
				ID: n.ID, MaxVolumes: 10, SupportsSingleNodeMultiWriter: true,
			}},
		}
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), n))
		nodes = append(nodes, n)
	}
	vol := structs.NewCSIVolume("vol", h.NextIndex())
	vol.PluginID = "foo"
	vol.Namespace = structs.DefaultNamespace
	vol.Schedulable = true
	vol.RequestedCapabilities = []*structs.CSIVolumeCapability{{
		AccessMode: structs.CSIVolumeAccessModeSingleNodeMultiWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
	}}
	must.NoError(t, h.State.UpsertCSIVolume(h.NextIndex(), []*structs.CSIVolume{vol}))

	job := mock.Job()
	tg := job.TaskGroups[0]
	tg.Count = 2
	tg.Volumes = map[string]*structs.VolumeRequest{"data": {
		Name: "data", Type: structs.VolumeTypeCSI, Source: "vol",
		AccessMode: structs.CSIVolumeAccessModeSingleNodeMultiWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
	}}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	job, err := h.State.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)

	plan := replProcess(t, h, job, structs.EvalTriggerJobRegister)
	must.NotNil(t, plan)
	placed := placedAllocs(plan)
	must.Len(t, 2, placed)
	distinct := map[string]bool{}
	for _, a := range placed {
		distinct[a.NodeID] = true
	}
	must.Eq(t, 1, len(distinct), must.Sprint("the allocations of the volume were placed on two nodes"))

	// The claims are made by the clients when the allocations start: both
	// are on the one node.
	claim := func(a *structs.Allocation, nodeID string) error {
		return h.State.CSIVolumeClaim(h.NextIndex(), structs.DefaultNamespace, "vol", &structs.CSIVolumeClaim{
			AllocationID: a.ID, NodeID: nodeID, Mode: structs.CSIVolumeClaimWrite, State: structs.CSIVolumeClaimStateTaken,
			AccessMode: structs.CSIVolumeAccessModeSingleNodeMultiWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		})
	}
	must.NoError(t, claim(placed[0], placed[0].NodeID))
	must.NoError(t, claim(placed[1], placed[1].NodeID))

	// A claim from another node is refused with an error the client does
	// not retry: the allocation has to be placed again.
	other := nodes[0].ID
	if other == placed[0].NodeID {
		other = nodes[1].ID
	}
	stray := mock.Alloc()
	stray.Job, stray.JobID, stray.NodeID = job, job.ID, other
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{stray}))
	err = claim(stray, other)
	must.ErrorContains(t, err, structs.ErrCSIVolumeClaimedOnOtherNode.Error())
	must.StrNotContains(t, err.Error(), structs.ErrCSIVolumeMaxClaims.Error())
}
