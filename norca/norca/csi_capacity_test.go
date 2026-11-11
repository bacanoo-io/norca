// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client"
	cconfig "github.com/bacanoo-io/norca/norca/client/config"
	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// TestCSICapacity_Tracker checks when a plugin is due and that a plugin
// without capacity any more is forgotten.
func TestCSICapacity_Tracker(t *testing.T) {
	ci.Parallel(t)

	c := newCSICapacity()
	now := time.Unix(1000, 0)
	must.True(t, c.due("p", time.Minute, now))
	c.set("p", []*structs.CSIPluginCapacity{{AvailableCapacity: 5}}, now)
	must.False(t, c.due("p", time.Minute, now.Add(30*time.Second)))
	must.True(t, c.due("p", time.Minute, now.Add(time.Minute)))

	got := c.get("p")
	must.Len(t, 1, got)
	got[0].AvailableCapacity = 99 // a copy
	must.Eq(t, int64(5), c.get("p")[0].AvailableCapacity)

	c.keep(map[string]bool{"other": true})
	must.SliceEmpty(t, c.get("p"))
	must.True(t, c.due("p", time.Minute, now))
}

// TestCSICapacity_PluginHelpers checks the capability, topology and gauge
// helpers.
func TestCSICapacity_PluginHelpers(t *testing.T) {
	ci.Parallel(t)

	p := &structs.CSIPlugin{
		ID: "p",
		Controllers: map[string]*structs.CSIInfo{
			"n1": {Healthy: true, ControllerInfo: &structs.CSIControllerInfo{SupportsGetCapacity: true}},
		},
		Nodes: map[string]*structs.CSIInfo{
			"n1": {NodeInfo: &structs.CSINodeInfo{AccessibleTopology: &structs.CSITopology{Segments: map[string]string{"zone": "b", "rack": "r1"}}}},
			"n2": {NodeInfo: &structs.CSINodeInfo{AccessibleTopology: &structs.CSITopology{Segments: map[string]string{"zone": "a"}}}},
			"n3": {NodeInfo: &structs.CSINodeInfo{AccessibleTopology: &structs.CSITopology{Segments: map[string]string{"zone": "a"}}}},
			"n4": {NodeInfo: &structs.CSINodeInfo{}},
		},
	}
	must.True(t, pluginSupportsGetCapacity(p))
	must.False(t, pluginUsesTopologies(p))
	p.Nodes["n1"].RequiresTopologies = true
	must.True(t, pluginUsesTopologies(p))

	topos := pluginNodeTopologies(p)
	must.Len(t, 2, topos)
	must.Eq(t, "rack=r1,zone=b", structs.CSITopologyKey(topos[0]))
	must.Eq(t, "zone=a", structs.CSITopologyKey(topos[1]))

	p.Controllers["n1"].Healthy = false
	must.False(t, pluginSupportsGetCapacity(p))

	gauges := csiCapacityGauges("p", []*structs.CSIPluginCapacity{
		{AvailableCapacity: 100, MaximumVolumeSize: pointer.Of(int64(50))},
		{Topology: topos[1], AvailableCapacity: 10},
		{Topology: topos[0], Error: "boom"},
	})
	must.Len(t, 3, gauges)
	for _, g := range gauges {
		_, ok := metricsstore.GaugeLabels(g.Metric)
		must.True(t, ok, must.Sprint(g.Metric))
	}
	must.Eq(t, map[string]string{"plugin": "p", "topology": ""}, gauges[0].Labels)
	must.Eq(t, metricsstore.MetricCSIMaximumVolumeSize, gauges[1].Metric)
	must.Eq(t, map[string]string{"plugin": "p", "topology": "zone=a"}, gauges[2].Labels)
}

// TestCSICapacity_Server checks the capacity end to end on a server with a
// client that runs the controller: the leader's poll and the plugin API, the
// plugin capacity RPC, and volume create refused when the plugin reports
// less than capacity_min.
func TestCSICapacity_Server(t *testing.T) {
	ci.Parallel(t)

	srv, shutdown := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer shutdown()
	testutil.WaitForLeader(t, srv.RPC)

	fake := newMockClientCSI()
	fake.NextCreateResponse = &cstructs.ClientCSIControllerCreateVolumeResponse{ExternalVolumeID: "ext-1", CapacityBytes: 1 << 30}
	fake.NextGetCapacityResponse = &cstructs.ClientCSIControllerGetCapacityResponse{
		AvailableCapacity: 2 << 30,
		MaximumVolumeSize: pointer.Of(int64(4 << 30)),
	}
	c, cleanup := client.TestClientWithRPCs(t,
		func(c *cconfig.Config) { c.Servers = []string{srv.config.RPCAddr.String()} },
		map[string]interface{}{"CSI": fake},
	)
	defer cleanup()

	node := c.UpdateConfig(func(cfg *cconfig.Config) {
		cfg.Node.Attributes["nomad.version"] = "0.11.0"
	}).Node
	var regResp structs.NodeUpdateResponse
	must.NoError(t, c.RPC("Node.Register", &structs.NodeRegisterRequest{
		Node: node, WriteRequest: structs.WriteRequest{Region: "global"},
	}, &regResp))
	testutil.WaitForResult(func() (bool, error) {
		return len(srv.connectedNodes()) == 1, nil
	}, func(err error) { t.Fatalf("should have a client") })

	node = c.UpdateConfig(func(cfg *cconfig.Config) {
		cfg.Node.CSIControllerPlugins = map[string]*structs.CSIInfo{
			"minnie": {
				PluginID: "minnie",
				AllocID:  uuid.Generate(),
				Healthy:  true,
				ControllerInfo: &structs.CSIControllerInfo{
					SupportsCreateDelete: true,
					SupportsGetCapacity:  true,
				},
				RequiresControllerPlugin: true,
			},
		}
		cfg.Node.CSINodePlugins = map[string]*structs.CSIInfo{
			"minnie": {PluginID: "minnie", AllocID: uuid.Generate(), Healthy: true, NodeInfo: &structs.CSINodeInfo{}},
		}
	}).Node
	must.NoError(t, srv.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	// The leader's poll: the whole storage only (no topologies), kept for
	// the plugin API.
	now := time.Now()
	must.NoError(t, srv.pollCSICapacity(now))
	must.Len(t, 1, fake.GetCapacityRequests)
	must.Nil(t, fake.GetCapacityRequests[0].Topology)
	var getResp structs.CSIPluginGetResponse
	err := srv.RPC("CSIPlugin.Get", &structs.CSIPluginGetRequest{
		ID: "minnie", QueryOptions: structs.QueryOptions{Region: "global"},
	}, &getResp)
	must.NoError(t, err, must.Sprint(err))
	must.NotNil(t, getResp.Plugin)
	must.Len(t, 1, getResp.Plugin.Capacity)
	must.Eq(t, int64(2<<30), getResp.Plugin.Capacity[0].AvailableCapacity)

	// Not due again within the interval.
	must.NoError(t, srv.pollCSICapacity(now.Add(time.Minute)))
	must.Len(t, 1, fake.GetCapacityRequests)

	// norca plugin capacity: the parameters reach the controller.
	var capResp structs.CSIPluginCapacityResponse
	err = srv.RPC("CSIPlugin.Capacity", &structs.CSIPluginCapacityRequest{
		ID:           "minnie",
		Parameters:   map[string]string{"tier": "fast"},
		QueryOptions: structs.QueryOptions{Region: "global"},
	}, &capResp)
	must.NoError(t, err, must.Sprint(err))
	must.Eq(t, int64(2<<30), capResp.Capacity.AvailableCapacity)
	must.Eq(t, map[string]string{"tier": "fast"}, fake.GetCapacityRequests[1].Parameters)

	// A topology for a plugin without topologies is refused.
	err = srv.RPC("CSIPlugin.Capacity", &structs.CSIPluginCapacityRequest{
		ID:           "minnie",
		Topology:     &structs.CSITopology{Segments: map[string]string{"zone": "a"}},
		QueryOptions: structs.QueryOptions{Region: "global"},
	}, &capResp)
	must.ErrorContains(t, err, "no topologies")

	create := func(min int64) error {
		vol := &structs.CSIVolume{
			ID:                   uuid.Generate(),
			Name:                 "vol",
			PluginID:             "minnie",
			Parameters:           map[string]string{"tier": "fast"},
			RequestedCapacityMin: min,
			RequestedCapabilities: []*structs.CSIVolumeCapability{{
				AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
				AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
			}},
		}
		var resp structs.CSIVolumeCreateResponse
		return srv.RPC("CSIVolume.Create", &structs.CSIVolumeCreateRequest{
			Volumes:      []*structs.CSIVolume{vol},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace},
		}, &resp)
	}

	// More than available: refused before CreateVolume.
	must.ErrorContains(t, create(3<<30), "not enough capacity")
	must.Eq(t, 0, fake.CreateCalls)
	last := fake.GetCapacityRequests[len(fake.GetCapacityRequests)-1]
	must.Eq(t, map[string]string{"tier": "fast"}, last.Parameters)
	must.Len(t, 1, last.VolumeCapabilities)

	// Above the largest volume size although available.
	fake.NextGetCapacityResponse = &cstructs.ClientCSIControllerGetCapacityResponse{
		AvailableCapacity: 10 << 30,
		MaximumVolumeSize: pointer.Of(int64(1 << 30)),
	}
	must.ErrorContains(t, create(2<<30), "at most")
	must.Eq(t, 0, fake.CreateCalls)

	// Enough: created.
	must.NoError(t, create(1<<30))
	must.Eq(t, 1, fake.CreateCalls)

	// A failing capacity request does not block the create.
	fake.NextGetCapacityError = errNotEnoughCapacity
	fake.NextGetCapacityResponse = nil
	must.NoError(t, create(1<<30))
	must.Eq(t, 2, fake.CreateCalls)
}
