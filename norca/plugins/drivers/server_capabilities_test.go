// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package drivers

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/plugins/drivers/proto"
	"github.com/shoenig/test/must"
	"google.golang.org/grpc"
)

// capsDriver is a driver that has only its capabilities.
type capsDriver struct {
	DriverPlugin
	caps *Capabilities
}

func (d capsDriver) Capabilities() (*Capabilities, error) { return d.caps, nil }

// capsClient hands the request of the client to the server, as gRPC does.
type capsClient struct {
	proto.DriverClient
	server *driverPluginServer
}

func (c capsClient) Capabilities(ctx context.Context, req *proto.CapabilitiesRequest, _ ...grpc.CallOption) (*proto.CapabilitiesResponse, error) {
	return c.server.Capabilities(ctx, req)
}

// The mount and log collection capabilities of an external driver reach the
// client (review finding PLG-2).
func TestCapabilities_MountsAndLogCollectionTravel(t *testing.T) {
	for _, want := range []*Capabilities{
		{FSIsolation: FSIsolationUnveil, MountConfigs: MountConfigSupportNone, DisableLogCollection: true},
		{FSIsolation: FSIsolationChroot, MountConfigs: MountConfigSupportAll},
	} {
		server := &driverPluginServer{impl: capsDriver{caps: want}}
		client := &driverPluginClient{client: capsClient{server: server}, doneCtx: context.Background()}
		got, err := client.Capabilities()
		must.NoError(t, err)
		must.Eq(t, want.MountConfigs, got.MountConfigs)
		must.Eq(t, want.DisableLogCollection, got.DisableLogCollection)
	}
}
