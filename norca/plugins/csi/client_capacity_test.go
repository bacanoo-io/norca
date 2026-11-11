// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csi

import (
	"context"
	"testing"

	csipbv1 "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/shoenig/test/must"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bacanoo-io/norca/norca/ci"
)

// TestClient_RPC_ControllerGetCapacity checks the GetCapacity request (the
// capabilities, parameters and the topology only when given) and the
// optional sizes of the answer (clean room: CSI spec v1.7.0 GetCapacity).
func TestClient_RPC_ControllerGetCapacity(t *testing.T) {
	ci.Parallel(t)

	_, cc, _, client := newTestClient(t)

	cc.NextGetCapacityResponse = &csipbv1.GetCapacityResponse{
		AvailableCapacity: 100,
		MaximumVolumeSize: &wrappers.Int64Value{Value: 40},
	}
	resp, err := client.ControllerGetCapacity(context.Background(), &ControllerGetCapacityRequest{
		Capabilities: []*VolumeCapability{{AccessType: VolumeAccessTypeMount, AccessMode: VolumeAccessModeSingleNodeWriter}},
		Parameters:   map[string]string{"tier": "fast"},
		Topology:     &Topology{Segments: map[string]string{"zone": "a"}},
	})
	must.NoError(t, err)
	must.Eq(t, int64(100), resp.AvailableCapacity)
	must.Eq(t, int64(40), *resp.MaximumVolumeSize)
	must.Nil(t, resp.MinimumVolumeSize)

	req := cc.LastGetCapacityRequest
	must.Len(t, 1, req.VolumeCapabilities)
	must.Eq(t, map[string]string{"tier": "fast"}, req.Parameters)
	must.Eq(t, map[string]string{"zone": "a"}, req.AccessibleTopology.Segments)

	// Without anything: the whole storage, no topology.
	cc.NextGetCapacityResponse = &csipbv1.GetCapacityResponse{AvailableCapacity: 7, MinimumVolumeSize: &wrappers.Int64Value{Value: 1}}
	resp, err = client.ControllerGetCapacity(context.Background(), &ControllerGetCapacityRequest{})
	must.NoError(t, err)
	must.Eq(t, int64(1), *resp.MinimumVolumeSize)
	must.Nil(t, cc.LastGetCapacityRequest.AccessibleTopology)
	must.SliceEmpty(t, cc.LastGetCapacityRequest.VolumeCapabilities)

	cc.NextErr = status.Error(codes.Internal, "boom")
	_, err = client.ControllerGetCapacity(context.Background(), &ControllerGetCapacityRequest{})
	must.ErrorContains(t, err, "internal error")
}
