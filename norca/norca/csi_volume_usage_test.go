// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// TestCSIVolumeUsage_Update checks the latest usage per volume and node and
// the condition changes: abnormal, normal again, an unknown condition that
// keeps the last known one, a late report, and a volume that is gone.
func TestCSIVolumeUsage_Update(t *testing.T) {
	ci.Parallel(t)

	c := newCSIVolumeUsage()
	at := time.Unix(1000, 0)
	u := func(abnormal bool, at time.Time) *structs.CSIVolumeNodeUsage {
		return &structs.CSIVolumeNodeUsage{Namespace: "default", VolumeID: "vol", PluginID: "p", NodeID: "n1",
			ConditionKnown: true, Abnormal: abnormal, At: at}
	}

	must.SliceEmpty(t, c.update("n1", []*structs.CSIVolumeNodeUsage{u(false, at)}))

	changes := c.update("n1", []*structs.CSIVolumeNodeUsage{u(true, at.Add(time.Minute))})
	must.Len(t, 1, changes)
	must.True(t, changes[0].abnormal)

	// A late report does not change anything.
	must.SliceEmpty(t, c.update("n1", []*structs.CSIVolumeNodeUsage{u(false, at)}))
	must.True(t, c.forVolume("default", "vol")[0].Abnormal)

	// No condition (an error): the last known one stays.
	unknown := &structs.CSIVolumeNodeUsage{Namespace: "default", VolumeID: "vol", NodeID: "n1", Error: "boom", At: at.Add(2 * time.Minute)}
	must.SliceEmpty(t, c.update("n1", []*structs.CSIVolumeNodeUsage{unknown}))
	got := c.forVolume("default", "vol")
	must.Len(t, 1, got)
	must.True(t, got[0].Abnormal)
	must.Eq(t, "boom", got[0].Error)

	changes = c.update("n1", []*structs.CSIVolumeNodeUsage{u(false, at.Add(3*time.Minute))})
	must.Len(t, 1, changes)
	must.False(t, changes[0].abnormal)

	// Abnormal again, then left out of a report of the node: recovered.
	must.Len(t, 1, c.update("n1", []*structs.CSIVolumeNodeUsage{u(true, at.Add(4*time.Minute))}))
	must.SliceEmpty(t, c.update("n2", nil)) // another node changes nothing
	must.Len(t, 1, c.forVolume("default", "vol"))
	changes = c.update("n1", nil)
	must.Len(t, 1, changes)
	must.False(t, changes[0].abnormal)
	must.Eq(t, "vol", changes[0].usage.VolumeID)
	must.SliceEmpty(t, c.forVolume("default", "vol"))
}

// TestCSIVolumeUsage_Gauges checks the gauges of a report: only the units
// the plugin reported.
func TestCSIVolumeUsage_Gauges(t *testing.T) {
	ci.Parallel(t)

	gauges := volumeUsageGauges([]*structs.CSIVolumeNodeUsage{{
		Namespace: "default", VolumeID: "vol", PluginID: "p", NodeID: "n1",
		BytesKnown: true, BytesUsed: 1, BytesAvailable: 2, BytesTotal: 3,
		ConditionKnown: true, Abnormal: true,
	}})
	byMetric := map[string]float64{}
	for _, g := range gauges {
		_, ok := metricsstore.GaugeLabels(g.Metric)
		must.True(t, ok, must.Sprint(g.Metric))
		must.Eq(t, map[string]string{"ns": "default", "volume": "vol", "plugin": "p", "node": "n1"}, g.Labels)
		byMetric[g.Metric] = g.Value
	}
	must.Eq(t, map[string]float64{
		metricsstore.MetricVolumeUsedBytes:      1,
		metricsstore.MetricVolumeAvailableBytes: 2,
		metricsstore.MetricVolumeCapacityBytes:  3,
		metricsstore.MetricVolumeAbnormal:       1,
	}, byMetric)
}

// TestServer_RecordVolumeUsage checks a report on a server: only volumes the
// node has a claim on with the plugin of the report are kept, the volume API
// returns them, and the leader publishes the condition changes on the
// CSIVolume topic.
func TestServer_RecordVolumeUsage(t *testing.T) {
	ci.Parallel(t)

	srv, shutdown := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer shutdown()
	testutil.WaitForLeader(t, srv.RPC)

	store := srv.fsm.State()
	node := mock.Node()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 100, node))

	claimed, unclaimed := uuid.Generate(), uuid.Generate()
	vols := []*structs.CSIVolume{
		{ID: claimed, Namespace: structs.DefaultNamespace, PluginID: "p",
			ReadClaims: map[string]*structs.CSIVolumeClaim{"alloc": {AllocationID: "alloc", NodeID: node.ID}}},
		{ID: unclaimed, Namespace: structs.DefaultNamespace, PluginID: "p"},
	}
	must.NoError(t, store.UpsertCSIVolume(101, vols))

	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicCSIVolume: {"*"}},
		Namespace: structs.DefaultNamespace,
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	now := time.Now()
	report := &structs.TaskGroupUsageReport{
		NodeID:          node.ID,
		VolumesReported: true,
		Volumes: []*structs.CSIVolumeNodeUsage{
			{VolumeID: claimed, PluginID: "p", BytesKnown: true, BytesUsed: 5, ConditionKnown: true, Abnormal: true, Message: "bad", At: now},
			{VolumeID: claimed, Namespace: "other", PluginID: "p", At: now},
			{VolumeID: unclaimed, PluginID: "p", ConditionKnown: true, Abnormal: true, At: now},
		},
	}
	srv.recordVolumeUsage(report, now)

	usage := srv.csiVolumeUsage.forVolume(structs.DefaultNamespace, claimed)
	must.Len(t, 1, usage)
	must.Eq(t, node.ID, usage[0].NodeID)
	must.Eq(t, int64(5), usage[0].BytesUsed)
	must.SliceEmpty(t, srv.csiVolumeUsage.forVolume(structs.DefaultNamespace, unclaimed))
	must.SliceEmpty(t, srv.csiVolumeUsage.forVolume("other", claimed))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := sub.Next(ctx)
	must.NoError(t, err)
	must.Len(t, 1, events.Events)
	ev := events.Events[0]
	must.Eq(t, structs.TypeCSIVolumeAbnormal, ev.Type)
	must.Eq(t, claimed, ev.Key)
	payload := ev.Payload.(*structs.CSIVolumeConditionEvent)
	must.Eq(t, node.ID, payload.NodeID)
	must.Eq(t, "bad", payload.Message)

	// The volume API returns the usage of the nodes.
	var resp structs.CSIVolumeGetResponse
	must.NoError(t, srv.RPC("CSIVolume.Get", &structs.CSIVolumeGetRequest{
		ID:           claimed,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace},
	}, &resp))
	must.NotNil(t, resp.Volume)
	must.Len(t, 1, resp.Volume.NodeUsage)
	must.True(t, resp.Volume.NodeUsage[0].Abnormal)

	// A report without volumes: the volume is gone from the node.
	srv.recordVolumeUsage(&structs.TaskGroupUsageReport{NodeID: node.ID, VolumesReported: true}, now.Add(time.Minute))
	events, err = sub.Next(ctx)
	must.NoError(t, err)
	must.Eq(t, structs.TypeCSIVolumeRecovered, events.Events[0].Type)

	// A later part of a split report (no volumes reported) changes nothing.
	srv.recordVolumeUsage(report, now.Add(2*time.Minute))
	srv.recordVolumeUsage(&structs.TaskGroupUsageReport{NodeID: node.ID}, now.Add(3*time.Minute))
	must.Len(t, 1, srv.csiVolumeUsage.forVolume(structs.DefaultNamespace, claimed))
}
