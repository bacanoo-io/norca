// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client"
	cconfig "github.com/bacanoo-io/norca/norca/client/config"
	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// TestCSIVolumeStates_Record checks the drift decisions: a drift is published
// when two checks in a row see it, once, and again after it went away; a
// controller without the published nodes never drifts; the condition change.
func TestCSIVolumeStates_Record(t *testing.T) {
	ci.Parallel(t)

	c := newCSIVolumeStates()
	k := volumeKey{"default", "v"}
	now := time.Unix(1000, 0)
	drifted := &structs.CSIVolumeControllerState{PublishedNodesKnown: true, MissingExternalNodeIDs: []string{"n1"}}
	clean := &structs.CSIVolumeControllerState{PublishedNodesKnown: true}

	_, _, d := c.record(k, drifted.Copy(), now)
	must.Eq(t, driftPending, d)
	must.True(t, c.due(k, now), must.Sprint("checked again at the next tick"))
	_, _, d = c.record(k, drifted.Copy(), now)
	must.Eq(t, driftPublish, d)
	must.False(t, c.due(k, now.Add(time.Minute)))
	_, _, d = c.record(k, drifted.Copy(), now)
	must.Eq(t, driftNone, d, must.Sprint("published once"))
	_, _, d = c.record(k, clean.Copy(), now)
	must.Eq(t, driftNone, d)
	_, _, d = c.record(k, drifted.Copy(), now)
	must.Eq(t, driftPending, d, must.Sprint("a new drift after it went away"))
	_, _, d = c.record(k, clean.Copy(), now)
	must.Eq(t, driftNone, d, must.Sprint("seen once, then gone: nothing"))

	unknown := &structs.CSIVolumeControllerState{MissingExternalNodeIDs: []string{"n1"}}
	_, _, d = c.record(k, unknown, now)
	must.Eq(t, driftNone, d)

	changed, abnormal, _ := c.record(k, &structs.CSIVolumeControllerState{ConditionKnown: true, Abnormal: true, Message: "bad"}, now)
	must.True(t, changed)
	must.True(t, abnormal)
	changed, _, _ = c.record(k, &structs.CSIVolumeControllerState{Error: "boom"}, now)
	must.False(t, changed)
	must.True(t, c.get(k).Abnormal, must.Sprint("the last known condition is kept through an error"))
	changed, abnormal, _ = c.record(k, &structs.CSIVolumeControllerState{ConditionKnown: true}, now)
	must.True(t, changed)
	must.False(t, abnormal)

	c.keep(nil)
	must.Nil(t, c.get(k))
}

// TestCSIVolumeStates_ControllerState checks the comparison of the published
// nodes with the taken claims.
func TestCSIVolumeStates_ControllerState(t *testing.T) {
	ci.Parallel(t)

	vol := &structs.CSIVolume{
		ReadClaims: map[string]*structs.CSIVolumeClaim{
			"a1": {ExternalNodeID: "n1", State: structs.CSIVolumeClaimStateTaken},
			"a2": {ExternalNodeID: "n2", State: structs.CSIVolumeClaimStateNodeDetached},
		},
		WriteClaims: map[string]*structs.CSIVolumeClaim{
			"a3": {ExternalNodeID: "n3", State: structs.CSIVolumeClaimStateTaken},
			// A taken claim keeps no external ID: it comes from the node.
			"a4": {NodeID: "node-4", State: structs.CSIVolumeClaimStateTaken},
		},
	}
	lookup := func(nodeID, pluginID string) string {
		if nodeID == "node-4" {
			return "n4"
		}
		return ""
	}
	resp := &cstructs.ClientCSIControllerGetVolumeResponse{PublishedNodeIDs: []string{"n3", "n9", "n3"}}

	st := controllerState(vol, &structs.CSIControllerInfo{SupportsGet: true, SupportsListVolumesAttachedNodes: true}, resp, lookup, time.Now())
	must.True(t, st.PublishedNodesKnown)
	must.Eq(t, []string{"n3", "n9"}, st.PublishedExternalNodeIDs)
	must.Eq(t, []string{"n1", "n4"}, st.MissingExternalNodeIDs)
	must.Eq(t, []string{"n9"}, st.ExtraExternalNodeIDs)

	st = controllerState(vol, &structs.CSIControllerInfo{SupportsGet: true}, resp, lookup, time.Now())
	must.False(t, st.PublishedNodesKnown)
	must.SliceEmpty(t, st.MissingExternalNodeIDs)
}

// TestCSIVolumeStates_Server checks the live state on a server with a client
// that runs the controller: the leader's checks and VolumeDrift, the
// controller's condition events, the volume API and a live request.
func TestCSIVolumeStates_Server(t *testing.T) {
	ci.Parallel(t)

	srv, shutdown := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer shutdown()
	testutil.WaitForLeader(t, srv.RPC)

	fake := newMockClientCSI()
	fake.NextGetVolumeResponse = &cstructs.ClientCSIControllerGetVolumeResponse{
		ExternalVolumeID: "ext-1",
		PublishedNodeIDs: []string{"ghost"},
		ConditionKnown:   true,
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
					SupportsGet:                      true,
					SupportsListVolumesAttachedNodes: true,
					SupportsCondition:                true,
				},
				RequiresControllerPlugin: true,
			},
		}
		cfg.Node.CSINodePlugins = map[string]*structs.CSIInfo{
			"minnie": {PluginID: "minnie", AllocID: uuid.Generate(), Healthy: true, NodeInfo: &structs.CSINodeInfo{ID: "ext-n1"}},
		}
	}).Node
	store := srv.fsm.State()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	volID := uuid.Generate()
	must.NoError(t, store.UpsertCSIVolume(1001, []*structs.CSIVolume{{
		ID: volID, Namespace: structs.DefaultNamespace, PluginID: "minnie", ExternalID: "ext-1",
		ReadClaims: map[string]*structs.CSIVolumeClaim{
			"alloc": {AllocationID: "alloc", NodeID: node.ID, State: structs.CSIVolumeClaimStateTaken},
		},
	}}))

	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicCSIVolume: {"*"}},
		Namespace: structs.DefaultNamespace,
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	// First check: a drift seen once, nothing published.
	now := time.Now()
	must.NoError(t, srv.pollCSIVolumeStates(now))
	must.Eq(t, 1, fake.GetVolumeCalls)

	// Second check at the next tick: the drift is published.
	must.NoError(t, srv.pollCSIVolumeStates(now.Add(30*time.Second)))
	must.Eq(t, 2, fake.GetVolumeCalls)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := sub.Next(ctx)
	must.NoError(t, err)
	ev := events.Events[0]
	must.Eq(t, structs.TypeCSIVolumeDrift, ev.Type)
	payload := ev.Payload.(*structs.CSIVolumeDriftEvent)
	must.Eq(t, []string{"ext-n1"}, payload.MissingExternalNodeIDs)
	must.Eq(t, []string{"ghost"}, payload.ExtraExternalNodeIDs)

	// Not asked again before the interval.
	must.NoError(t, srv.pollCSIVolumeStates(now.Add(time.Minute)))
	must.Eq(t, 2, fake.GetVolumeCalls)

	// The volume API returns the leader's state.
	var getResp structs.CSIVolumeGetResponse
	err = srv.RPC("CSIVolume.Get", &structs.CSIVolumeGetRequest{
		ID: volID, QueryOptions: structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace},
	}, &getResp)
	must.NoError(t, err, must.Sprint(err))
	must.NotNil(t, getResp.Volume.ControllerState)
	must.Eq(t, []string{"ext-n1"}, getResp.Volume.ControllerState.MissingExternalNodeIDs)

	// A live request asks the controller at once: now published, abnormal.
	fake.NextGetVolumeResponse = &cstructs.ClientCSIControllerGetVolumeResponse{
		PublishedNodeIDs: []string{"ext-n1"},
		ConditionKnown:   true, Abnormal: true, Message: "degraded",
	}
	err = srv.RPC("CSIVolume.Get", &structs.CSIVolumeGetRequest{
		ID: volID, Live: true, QueryOptions: structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace},
	}, &getResp)
	must.NoError(t, err)
	must.Eq(t, 3, fake.GetVolumeCalls)
	must.SliceEmpty(t, getResp.Volume.ControllerState.MissingExternalNodeIDs)
	must.True(t, getResp.Volume.ControllerState.Abnormal)

	// The next check: the controller's condition changed: VolumeAbnormal
	// without a node.
	must.NoError(t, srv.pollCSIVolumeStates(now.Add(6*time.Minute)))
	events, err = sub.Next(ctx)
	must.NoError(t, err)
	ev = events.Events[0]
	must.Eq(t, structs.TypeCSIVolumeAbnormal, ev.Type)
	cond := ev.Payload.(*structs.CSIVolumeConditionEvent)
	must.Eq(t, "", cond.NodeID)
	must.Eq(t, "degraded", cond.Message)
}
