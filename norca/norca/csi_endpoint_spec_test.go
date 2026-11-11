// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
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

	// csi.proto, ControllerPublishVolumeRequest.readonly: "CO MUST set this
	// field to false if SP does not have the PUBLISH_READONLY controller
	// capability."
	must.False(t, controllerPublishReadOnly(structs.CSIVolumeClaimRead, without))
	must.True(t, controllerPublishReadOnly(structs.CSIVolumeClaimRead, with))
	must.False(t, controllerPublishReadOnly(structs.CSIVolumeClaimWrite, with))
}

func TestCSI_CreateSourceHint(t *testing.T) {
	ci.Parallel(t)

	plain := specPlugin(nil)
	clone := specPlugin(func(c *structs.CSIControllerInfo) { c.SupportsClone = true })
	snap := specPlugin(func(c *structs.CSIControllerInfo) { c.SupportsCreateDeleteSnapshot = true })

	must.Eq(t, "", createSourceHint(&structs.CSIVolume{ID: "v"}, plain))

	fromClone := &structs.CSIVolume{ID: "v", CloneID: "src"}
	must.StrContains(t, createSourceHint(fromClone, plain), "CLONE_VOLUME")
	must.Eq(t, "", createSourceHint(fromClone, clone))

	fromSnap := &structs.CSIVolume{ID: "v", SnapshotID: "snap-1"}
	must.StrContains(t, createSourceHint(fromSnap, plain), "CREATE_DELETE_SNAPSHOT")
	must.Eq(t, "", createSourceHint(fromSnap, snap))
}
