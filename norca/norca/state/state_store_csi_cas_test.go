// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// A register with an expected modify index is a check-and-set: a volume
// that was written since it was read is not written over, so a claim made in
// between stays.
func TestStateStore_UpsertCSIVolumeExpected(t *testing.T) {
	ci.Parallel(t)
	state := testStateStore(t)

	volume := func(id string) *structs.CSIVolume {
		return &structs.CSIVolume{
			ID: id, Namespace: structs.DefaultNamespace, PluginID: "minnie", ExternalID: "ext-" + id,
			AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
			AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		}
	}
	stored := func(id string) *structs.CSIVolume {
		t.Helper()
		vol, err := state.CSIVolumeByID(nil, structs.DefaultNamespace, id)
		must.NoError(t, err)
		return vol
	}

	// 0 expects that no volume is stored.
	must.NoError(t, state.UpsertCSIVolumeExpected(1000, []*structs.CSIVolume{volume("a")}, map[string]uint64{"a": 0}))
	must.Eq(t, 1000, stored("a").ModifyIndex)
	err := state.UpsertCSIVolumeExpected(1001, []*structs.CSIVolume{volume("a")}, map[string]uint64{"a": 0})
	must.True(t, structs.IsCSIVolumeConflictError(err), must.Sprintf("not a conflict: %v", err))
	must.ErrorContains(t, err, "has modify index 1000, expected 0")
	must.Eq(t, 1000, stored("a").ModifyIndex)

	// A volume that is not there has the modify index 0.
	err = state.UpsertCSIVolumeExpected(1002, []*structs.CSIVolume{volume("b")}, map[string]uint64{"b": 7})
	must.True(t, structs.IsCSIVolumeConflictError(err), must.Sprintf("not a conflict: %v", err))
	must.Nil(t, stored("b"))

	// A claim is committed after the volume was read: the register built
	// from the read is refused, and the claim stays.
	// (The claim is written as a volume with it: a real claim needs a
	// healthy plugin, and what counts here is that the volume was written.)
	read := stored("a")
	alloc := mock.Alloc()
	claimed := read.Copy()
	claimed.WriteClaims[alloc.ID] = &structs.CSIVolumeClaim{
		AllocationID: alloc.ID, NodeID: alloc.NodeID, Mode: structs.CSIVolumeClaimWrite,
		AccessMode: structs.CSIVolumeAccessModeSingleNodeWriter, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		State: structs.CSIVolumeClaimStateTaken,
	}
	must.NoError(t, state.UpsertCSIVolume(1004, []*structs.CSIVolume{claimed}))
	stale := read.Copy()
	stale.Capacity = 2 << 30
	err = state.UpsertCSIVolumeExpected(1005, []*structs.CSIVolume{stale}, map[string]uint64{"a": read.ModifyIndex})
	must.True(t, structs.IsCSIVolumeConflictError(err), must.Sprintf("not a conflict: %v", err))
	must.MapContainsKey(t, stored("a").WriteClaims, alloc.ID)
	must.Eq(t, 1004, stored("a").ModifyIndex)

	// Built from the volume as it is now, it is written, with the claim.
	fresh := stored("a").Copy()
	fresh.Capacity = 2 << 30
	must.NoError(t, state.UpsertCSIVolumeExpected(1006, []*structs.CSIVolume{fresh}, map[string]uint64{"a": 1004}))
	must.Eq(t, 2<<30, stored("a").Capacity)
	must.MapContainsKey(t, stored("a").WriteClaims, alloc.ID)
	must.Eq(t, 1000, stored("a").CreateIndex)

	// One failed check refuses the whole request.
	err = state.UpsertCSIVolumeExpected(1007, []*structs.CSIVolume{volume("c"), stale},
		map[string]uint64{"c": 0, "a": read.ModifyIndex})
	must.True(t, structs.IsCSIVolumeConflictError(err), must.Sprintf("not a conflict: %v", err))
	must.Nil(t, stored("c"))

	// Without an entry, and without the map, a volume is written unchecked
	// as before.
	must.NoError(t, state.UpsertCSIVolumeExpected(1008, []*structs.CSIVolume{stored("a").Copy()}, map[string]uint64{"other": 3}))
	must.NoError(t, state.UpsertCSIVolume(1009, []*structs.CSIVolume{stored("a").Copy()}))
	must.Eq(t, 1009, stored("a").ModifyIndex)
}
