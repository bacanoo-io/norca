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
)

// TestClient_RPC_ControllerGetVolume checks the conversion of the answer of
// ControllerGetVolume: the volume, the published nodes and the condition
// (clean room: CSI spec v1.7.0).
func TestClient_RPC_ControllerGetVolume(t *testing.T) {
	ci.Parallel(t)

	_, cc, _, client := newTestClient(t)

	_, err := client.ControllerGetVolume(context.Background(), &ControllerGetVolumeRequest{})
	must.ErrorContains(t, err, "missing ExternalVolumeID")

	cc.NextGetVolumeResponse = &csipbv1.ControllerGetVolumeResponse{
		Volume: &csipbv1.Volume{VolumeId: "ext-1", CapacityBytes: 42},
		Status: &csipbv1.ControllerGetVolumeResponse_VolumeStatus{
			PublishedNodeIds: []string{"n1", "n2"},
			VolumeCondition:  &csipbv1.VolumeCondition{Abnormal: true, Message: "bad"},
		},
	}
	resp, err := client.ControllerGetVolume(context.Background(), &ControllerGetVolumeRequest{ExternalVolumeID: "ext-1"})
	must.NoError(t, err)
	must.Eq(t, "ext-1", resp.Volume.ExternalVolumeID)
	must.Eq(t, int64(42), resp.Volume.CapacityBytes)
	must.Eq(t, []string{"n1", "n2"}, resp.PublishedNodeIDs)
	must.Eq(t, &VolumeCondition{Abnormal: true, Message: "bad"}, resp.Condition)

	cc.NextGetVolumeResponse = &csipbv1.ControllerGetVolumeResponse{Volume: &csipbv1.Volume{VolumeId: "ext-1"}}
	resp, err = client.ControllerGetVolume(context.Background(), &ControllerGetVolumeRequest{ExternalVolumeID: "ext-1"})
	must.NoError(t, err)
	must.Nil(t, resp.Condition)
	must.SliceEmpty(t, resp.PublishedNodeIDs)

	cc.NextErr = status.Error(codes.NotFound, "gone")
	_, err = client.ControllerGetVolume(context.Background(), &ControllerGetVolumeRequest{ExternalVolumeID: "ext-1"})
	must.ErrorContains(t, err, "not found by the controller")
}
