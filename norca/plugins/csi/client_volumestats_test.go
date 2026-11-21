// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csi

import (
	"context"
	"testing"

	csipbv1 "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/shoenig/test/must"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// ControllerGetVolume keeps the content source of the volume (review finding
// PLG-3).
func TestClient_RPC_ControllerGetVolume_ContentSource(t *testing.T) {
	ci.Parallel(t)

	_, cc, _, client := newTestClient(t)
	cc.NextGetVolumeResponse = &csipbv1.ControllerGetVolumeResponse{
		Volume: &csipbv1.Volume{VolumeId: "ext-1", ContentSource: &csipbv1.VolumeContentSource{
			Type: &csipbv1.VolumeContentSource_Snapshot{
				Snapshot: &csipbv1.VolumeContentSource_SnapshotSource{SnapshotId: "snap-1"},
			},
		}},
	}
	resp, err := client.ControllerGetVolume(context.Background(), &ControllerGetVolumeRequest{ExternalVolumeID: "ext-1"})
	must.NoError(t, err)
	must.NotNil(t, resp.Volume.ContentSource)
	must.Eq(t, "snap-1", resp.Volume.ContentSource.SnapshotID)
}

// NodeGetVolumeStats refuses a nil request and keeps the status of the error
// of the plugin (review finding PLG-4).
func TestClient_RPC_NodeGetVolumeStats_Errors(t *testing.T) {
	ci.Parallel(t)

	_, _, nc, client := newTestClient(t)
	_, err := client.NodeGetVolumeStats(context.Background(), nil)
	must.ErrorContains(t, err, "missing NodeGetVolumeStats request")

	req := &NodeGetVolumeStatsRequest{ExternalVolumeID: "ext-1", VolumePath: "/p"}
	for _, code := range []codes.Code{codes.Unimplemented, codes.DeadlineExceeded, codes.NotFound} {
		nc.NextErr = status.Error(code, "no")
		_, err = client.NodeGetVolumeStats(context.Background(), req)
		must.Error(t, err)
		st, ok := status.FromError(err)
		must.True(t, ok, must.Sprintf("code %s: %v", code, err))
		must.Eq(t, code, st.Code())
	}
	must.ErrorIs(t, err, structs.ErrCSIClientRPCIgnorable)
}
