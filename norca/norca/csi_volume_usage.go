// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sort"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The usage and the condition of the CSI volumes on the client nodes (Norca,
// clean room, design csi-completion-plan.md, phase 2). Clients send what
// their node plugins answer to NodeGetVolumeStats with their usage report on
// the system bus; every server reads every report and keeps the latest
// answer per volume and node in memory (never in Raft). The volume API
// returns them, the metrics store keeps their history as the norca.volume.*
// gauges, and the leader publishes a CSIVolume event when a node reports a
// volume abnormal or normal again.

// volumeNodeKey identifies the usage of a volume on a node.
type volumeNodeKey struct {
	namespace, volumeID, nodeID string
}

// csiVolumeUsage is the latest usage of the volumes, by volume and node.
type csiVolumeUsage struct {
	mu     sync.Mutex
	latest map[volumeNodeKey]*structs.CSIVolumeNodeUsage
}

func newCSIVolumeUsage() *csiVolumeUsage {
	return &csiVolumeUsage{latest: map[volumeNodeKey]*structs.CSIVolumeNodeUsage{}}
}

// conditionChange is a change of the condition of a volume on a node.
type conditionChange struct {
	usage    *structs.CSIVolumeNodeUsage
	abnormal bool
}

// update replaces the usage of the volumes of a node with those of a
// report: a volume the report leaves out is no longer published on the node.
// It returns the changes of the condition: a volume reported abnormal, and a
// volume abnormal before that is normal again or gone.
func (c *csiVolumeUsage) update(nodeID string, usages []*structs.CSIVolumeNodeUsage) []conditionChange {
	c.mu.Lock()
	defer c.mu.Unlock()

	var changes []conditionChange
	seen := map[volumeNodeKey]bool{}
	for _, u := range usages {
		key := volumeNodeKey{u.Namespace, u.VolumeID, nodeID}
		seen[key] = true
		prev := c.latest[key]
		if prev != nil && u.At.Before(prev.At) {
			continue // an older report delivered late
		}
		c.latest[key] = u
		if !u.ConditionKnown {
			// No condition this time (e.g. an error): keep the last one known.
			if prev != nil && prev.ConditionKnown {
				u.ConditionKnown, u.Abnormal, u.Message = true, prev.Abnormal, prev.Message
			}
			continue
		}
		wasAbnormal := prev != nil && prev.ConditionKnown && prev.Abnormal
		if u.Abnormal != wasAbnormal {
			changes = append(changes, conditionChange{usage: u, abnormal: u.Abnormal})
		}
	}
	for key, prev := range c.latest {
		if key.nodeID != nodeID || seen[key] {
			continue
		}
		delete(c.latest, key)
		if prev.ConditionKnown && prev.Abnormal {
			gone := prev.Copy()
			gone.Abnormal, gone.Message = false, "the volume is no longer published on the node"
			gone.At = time.Now()
			changes = append(changes, conditionChange{usage: gone, abnormal: false})
		}
	}
	return changes
}

// forVolume returns copies of the usage of a volume on its nodes, sorted by
// node.
func (c *csiVolumeUsage) forVolume(namespace, volumeID string) []*structs.CSIVolumeNodeUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*structs.CSIVolumeNodeUsage
	for key, u := range c.latest {
		if key.namespace == namespace && key.volumeID == volumeID {
			out = append(out, u.Copy())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// volumeClaimedOnNode reports whether the volume has a claim of the node (a
// claim that is not released yet), with the plugin of the report.
func volumeClaimedOnNode(vol *structs.CSIVolume, pluginID, nodeID string) bool {
	if vol == nil || vol.PluginID != pluginID {
		return false
	}
	for _, claims := range []map[string]*structs.CSIVolumeClaim{vol.ReadClaims, vol.WriteClaims} {
		for _, claim := range claims {
			if claim != nil && claim.NodeID == nodeID {
				return true
			}
		}
	}
	return false
}

// recordVolumeUsage takes the volume usage of a report: only volumes of the
// state that the node has a claim on are kept, so that a node can only
// report its own volumes. The metrics store gets the gauges, and the leader
// publishes the changes of the condition.
func (s *Server) recordVolumeUsage(report *structs.TaskGroupUsageReport, published time.Time) {
	if !report.VolumesReported || s.csiVolumeUsage == nil {
		return
	}
	snap := s.fsm.State()
	node, err := snap.NodeByID(nil, report.NodeID)
	if err != nil || node == nil {
		return
	}
	var valid []*structs.CSIVolumeNodeUsage
	for _, u := range report.Volumes {
		if u == nil || u.VolumeID == "" {
			continue
		}
		ns := u.Namespace
		if ns == "" {
			ns = structs.DefaultNamespace
		}
		vol, err := snap.CSIVolumeByID(nil, ns, u.VolumeID)
		if err != nil || !volumeClaimedOnNode(vol, u.PluginID, node.ID) {
			continue
		}
		own := u.Copy()
		own.Namespace = ns
		own.NodeID = node.ID
		if own.At.IsZero() || own.At.After(published) {
			own.At = published
		}
		valid = append(valid, own)
	}

	changes := s.csiVolumeUsage.update(node.ID, valid)

	if s.metricsStore != nil {
		s.metricsStore.RecordGauges(published, volumeUsageGauges(valid))
	}
	if len(changes) > 0 && s.IsLeader() {
		s.publishVolumeConditions(snap, changes)
	}
}

// volumeUsageGauges are the gauges of the usage of volumes on a node.
func volumeUsageGauges(usages []*structs.CSIVolumeNodeUsage) []metricsstore.Gauge {
	var out []metricsstore.Gauge
	for _, u := range usages {
		labels := map[string]string{"ns": u.Namespace, "volume": u.VolumeID, "plugin": u.PluginID, "node": u.NodeID}
		add := func(metric string, v int64) {
			out = append(out, metricsstore.Gauge{Metric: metric, Labels: labels, Value: float64(v)})
		}
		if u.BytesKnown {
			add(metricsstore.MetricVolumeUsedBytes, u.BytesUsed)
			add(metricsstore.MetricVolumeAvailableBytes, u.BytesAvailable)
			add(metricsstore.MetricVolumeCapacityBytes, u.BytesTotal)
		}
		if u.InodesKnown {
			add(metricsstore.MetricVolumeInodesUsed, u.InodesUsed)
			add(metricsstore.MetricVolumeInodesFree, u.InodesFree)
		}
		if u.ConditionKnown {
			var v int64
			if u.Abnormal {
				v = 1
			}
			add(metricsstore.MetricVolumeAbnormal, v)
		}
	}
	return out
}

// publishVolumeConditions publishes the changes of the condition of volumes
// on the CSIVolume topic. Like the other events of the leader that are not
// in Raft, they are published by the leader only.
func (s *Server) publishVolumeConditions(store *state.StateStore, changes []conditionChange) {
	if store == nil {
		return
	}
	broker, err := store.EventBroker()
	if err != nil {
		return
	}
	index, _ := store.LatestIndex()
	events := make([]structs.Event, 0, len(changes))
	for _, ch := range changes {
		typ := structs.TypeCSIVolumeRecovered
		if ch.abnormal {
			typ = structs.TypeCSIVolumeAbnormal
		}
		u := ch.usage
		events = append(events, structs.Event{
			Topic:      structs.TopicCSIVolume,
			Type:       typ,
			Key:        u.VolumeID,
			Namespace:  u.Namespace,
			FilterKeys: []string{u.NodeID, u.PluginID},
			Index:      index,
			Payload: &structs.CSIVolumeConditionEvent{
				Namespace: u.Namespace,
				VolumeID:  u.VolumeID,
				PluginID:  u.PluginID,
				NodeID:    u.NodeID,
				Abnormal:  ch.abnormal,
				Message:   u.Message,
				At:        u.At,
			},
		})
	}
	broker.Publish(&structs.Events{Index: index, Events: events})
}
