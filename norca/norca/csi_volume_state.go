// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The live state of the CSI volumes (Norca, clean room, design
// csi-completion-plan.md, phase 4; CSI spec v1.7.0 ControllerGetVolume).
// For volumes of a plugin with a GET_VOLUME controller the leader asks the
// controller every csiVolumeStateInterval for the state of the volume:
//
//   - With LIST_VOLUMES_PUBLISHED_NODES it compares the nodes the controller
//     lists as published with the claims of Norca. A claimed node the
//     controller does not list is a drift; when two checks in a row (the
//     second at the next tick) see the same drift, the leader publishes a
//     VolumeDrift event. Nodes the controller lists without a claim are only
//     shown: the specification allows them. Nothing is changed.
//   - With VOLUME_CONDITION a change of the condition the controller reports
//     publishes VolumeAbnormal or VolumeRecovered, with no node.
//
// The answers stay in the memory of the leader; volume status shows them,
// and volume status -live asks the controller at once.

var (
	// csiVolumeStateInterval is how often a volume is asked.
	csiVolumeStateInterval = 5 * time.Minute
	// csiVolumeStateTick is how often the leader looks for volumes to ask;
	// a drift seen once is checked again at the next tick.
	csiVolumeStateTick = 30 * time.Second
)

// volumeKey identifies a volume.
type volumeKey struct{ namespace, id string }

// csiVolumeStates is what the leader knows of the volumes' live state.
type csiVolumeStates struct {
	mu       sync.Mutex
	latest   map[volumeKey]*structs.CSIVolumeControllerState
	next     map[volumeKey]time.Time
	pending  map[volumeKey]string // a drift seen once
	reported map[volumeKey]string // the drift last published
}

func newCSIVolumeStates() *csiVolumeStates {
	return &csiVolumeStates{
		latest:   map[volumeKey]*structs.CSIVolumeControllerState{},
		next:     map[volumeKey]time.Time{},
		pending:  map[volumeKey]string{},
		reported: map[volumeKey]string{},
	}
}

func (c *csiVolumeStates) due(k volumeKey, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	next, ok := c.next[k]
	return !ok || !now.Before(next)
}

func (c *csiVolumeStates) get(k volumeKey) *structs.CSIVolumeControllerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest[k].Copy()
}

// driftDecision is what a check means for the drift of a volume.
type driftDecision int

const (
	driftNone    driftDecision = iota // no drift, or already published
	driftPending                      // seen once: check again soon
	driftPublish                      // seen twice in a row: publish
)

// record stores the answer of a check and returns the change of the
// controller's condition (changed, abnormal) and what to do about a drift.
func (c *csiVolumeStates) record(k volumeKey, st *structs.CSIVolumeControllerState, now time.Time) (bool, bool, driftDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()

	prev := c.latest[k]
	c.latest[k] = st
	c.next[k] = now.Add(csiVolumeStateInterval)

	changed, abnormal := false, false
	if st.Error == "" && st.ConditionKnown {
		wasAbnormal := prev != nil && prev.ConditionKnown && prev.Abnormal
		changed, abnormal = st.Abnormal != wasAbnormal, st.Abnormal
	} else if st.Error != "" && prev != nil {
		// Keep the last known condition through a failed check.
		st.ConditionKnown, st.Abnormal, st.Message = prev.ConditionKnown, prev.Abnormal, prev.Message
	}

	if st.Error != "" || !st.PublishedNodesKnown {
		return changed, abnormal, driftNone
	}
	sig := strings.Join(st.MissingExternalNodeIDs, ",")
	switch {
	case sig == "":
		delete(c.pending, k)
		delete(c.reported, k)
		return changed, abnormal, driftNone
	case c.reported[k] == sig:
		return changed, abnormal, driftNone
	case c.pending[k] == sig:
		delete(c.pending, k)
		c.reported[k] = sig
		return changed, abnormal, driftPublish
	default:
		c.pending[k] = sig
		c.next[k] = now // again at the next tick
		return changed, abnormal, driftPending
	}
}

// keep drops every volume but those of keys.
func (c *csiVolumeStates) keep(keys map[volumeKey]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range []map[volumeKey]string{c.pending, c.reported} {
		for k := range m {
			if !keys[k] {
				delete(m, k)
			}
		}
	}
	for k := range c.latest {
		if !keys[k] {
			delete(c.latest, k)
			delete(c.next, k)
		}
	}
}

// pluginControllerInfo returns the controller info of a healthy controller of
// the plugin that has the capability, nil if none has.
func pluginControllerInfo(p *structs.CSIPlugin, has func(*structs.CSIControllerInfo) bool) *structs.CSIControllerInfo {
	if p == nil {
		return nil
	}
	for _, info := range p.Controllers {
		if info != nil && info.Healthy && info.ControllerInfo != nil && has(info.ControllerInfo) {
			return info.ControllerInfo
		}
	}
	return nil
}

func supportsGetVolume(ci *structs.CSIControllerInfo) bool { return ci.SupportsGet }

// externalNodeIDFunc returns the external ID (the ID the plugin knows) of a
// node for a plugin, "" if unknown.
type externalNodeIDFunc func(nodeID, pluginID string) string

// stateExternalNodeID looks the external ID up in the fingerprint of the node
// plugin of the node, as the controller publish does.
func stateExternalNodeID(snap *state.StateSnapshot) externalNodeIDFunc {
	return func(nodeID, pluginID string) string {
		node, err := snap.NodeByID(nil, nodeID)
		if err != nil || node == nil {
			return ""
		}
		info := node.CSINodePlugins[pluginID]
		if info == nil || info.NodeInfo == nil {
			return ""
		}
		return info.NodeInfo.ID
	}
}

// claimedExternalNodeIDs returns the external IDs of the nodes with a taken
// claim on the volume, sorted. A claim keeps the external ID only once it was
// unpublished; for a taken claim it comes from the node (lookup).
func claimedExternalNodeIDs(vol *structs.CSIVolume, lookup externalNodeIDFunc) []string {
	seen := map[string]bool{}
	for _, claims := range []map[string]*structs.CSIVolumeClaim{vol.ReadClaims, vol.WriteClaims} {
		for _, claim := range claims {
			if claim == nil || claim.State != structs.CSIVolumeClaimStateTaken {
				continue
			}
			id := claim.ExternalNodeID
			if id == "" && lookup != nil {
				id = lookup(claim.NodeID, vol.PluginID)
			}
			if id != "" {
				seen[id] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// controllerState builds the state of a volume from the answer of the
// controller.
func controllerState(vol *structs.CSIVolume, ci *structs.CSIControllerInfo, resp *cstructs.ClientCSIControllerGetVolumeResponse, lookup externalNodeIDFunc, now time.Time) *structs.CSIVolumeControllerState {
	st := &structs.CSIVolumeControllerState{
		CapacityBytes:  resp.CapacityBytes,
		ConditionKnown: resp.ConditionKnown,
		Abnormal:       resp.Abnormal,
		Message:        resp.Message,
		At:             now,
	}
	published := append([]string(nil), resp.PublishedNodeIDs...)
	sort.Strings(published)
	published = slices.Compact(published)
	st.PublishedExternalNodeIDs = published
	if ci != nil && ci.SupportsListVolumesAttachedNodes {
		st.PublishedNodesKnown = true
		claimed := claimedExternalNodeIDs(vol, lookup)
		for _, id := range claimed {
			if !slices.Contains(published, id) {
				st.MissingExternalNodeIDs = append(st.MissingExternalNodeIDs, id)
			}
		}
		for _, id := range published {
			if !slices.Contains(claimed, id) {
				st.ExtraExternalNodeIDs = append(st.ExtraExternalNodeIDs, id)
			}
		}
	}
	return st
}

// askVolumeState asks a controller of the plugin for the state of a volume.
func (s *Server) askVolumeState(vol *structs.CSIVolume, plugin *structs.CSIPlugin, now time.Time) *structs.CSIVolumeControllerState {
	ci := pluginControllerInfo(plugin, supportsGetVolume)
	req := &cstructs.ClientCSIControllerGetVolumeRequest{ExternalVolumeID: vol.RemoteID()}
	req.PluginID = plugin.ID
	var resp cstructs.ClientCSIControllerGetVolumeResponse
	if err := s.RPC("ClientCSI.ControllerGetVolume", req, &resp); err != nil {
		return &structs.CSIVolumeControllerState{Error: err.Error(), At: now}
	}
	snap, err := s.fsm.State().Snapshot()
	if err != nil {
		return &structs.CSIVolumeControllerState{Error: err.Error(), At: now}
	}
	return controllerState(vol, ci, &resp, stateExternalNodeID(snap), now)
}

// csiVolumeStateLoop asks the controllers for the state of the volumes while
// this server is the leader.
func (s *Server) csiVolumeStateLoop(stopCh chan struct{}) {
	tick := time.NewTicker(csiVolumeStateTick)
	defer tick.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-s.shutdownCtx.Done():
			return
		case now := <-tick.C:
			if err := s.pollCSIVolumeStates(now); err != nil {
				s.logger.Warn("CSI volume state: reading the volumes failed", "error", err)
			}
		}
	}
}

// pollCSIVolumeStates asks for every volume whose check is due.
func (s *Server) pollCSIVolumeStates(now time.Time) error {
	store := s.fsm.State()
	snap, err := store.Snapshot()
	if err != nil {
		return err
	}
	iter, err := snap.CSIVolumes(nil)
	if err != nil {
		return err
	}
	plugins := map[string]*structs.CSIPlugin{}
	seen := map[volumeKey]bool{}
	var conditions []conditionChange
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		vol := raw.(*structs.CSIVolume)
		plugin, ok := plugins[vol.PluginID]
		if !ok {
			plugin, _ = snap.CSIPluginByID(nil, vol.PluginID)
			plugins[vol.PluginID] = plugin
		}
		if vol.RemoteID() == "" || pluginControllerInfo(plugin, supportsGetVolume) == nil {
			continue
		}
		k := volumeKey{vol.Namespace, vol.ID}
		seen[k] = true
		if !s.csiVolumeStates.due(k, now) {
			continue
		}
		st := s.askVolumeState(vol, plugin, now)
		changed, abnormal, drift := s.csiVolumeStates.record(k, st, now)
		if changed {
			conditions = append(conditions, conditionChange{
				usage: &structs.CSIVolumeNodeUsage{
					Namespace: vol.Namespace, VolumeID: vol.ID, PluginID: vol.PluginID,
					Message: st.Message, At: now,
				},
				abnormal: abnormal,
			})
		}
		if drift == driftPublish {
			s.publishVolumeDrift(store, vol, st)
		}
	}
	s.csiVolumeStates.keep(seen)
	if len(conditions) > 0 {
		s.publishVolumeConditions(store, conditions)
	}
	return nil
}

// publishVolumeDrift publishes a VolumeDrift event on the CSIVolume topic,
// on the leader only, like the other events of the topic.
func (s *Server) publishVolumeDrift(store *state.StateStore, vol *structs.CSIVolume, st *structs.CSIVolumeControllerState) {
	broker, err := store.EventBroker()
	if err != nil {
		return
	}
	index, _ := store.LatestIndex()
	broker.Publish(&structs.Events{
		Index: index,
		Events: []structs.Event{{
			Topic:      structs.TopicCSIVolume,
			Type:       structs.TypeCSIVolumeDrift,
			Key:        vol.ID,
			Namespace:  vol.Namespace,
			FilterKeys: []string{vol.PluginID},
			Index:      index,
			Payload: &structs.CSIVolumeDriftEvent{
				Namespace:              vol.Namespace,
				VolumeID:               vol.ID,
				PluginID:               vol.PluginID,
				MissingExternalNodeIDs: st.MissingExternalNodeIDs,
				ExtraExternalNodeIDs:   st.ExtraExternalNodeIDs,
				At:                     st.At,
			},
		}},
	})
}
