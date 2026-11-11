// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/go-metrics"

	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The capacity of the storage of the CSI plugins (Norca, clean room, design
// csi-completion-plan.md, phase 3; CSI spec v1.7.0 GetCapacity). Every
// capacity_interval of a plugin (csi_plugin { capacity_interval }, 5m by
// default, 0 off) the leader asks one of its controllers with GET_CAPACITY for
// the capacity of the whole storage and, for a plugin with topologies, of
// every topology segment of its nodes. The latest answers are kept in the
// memory of the leader (never in Raft): the plugin API returns them and the
// metrics store of the leader keeps their history as the norca.csi.* gauges.

// csiCapacityTick is how often the leader looks for plugins whose capacity is
// due.
var csiCapacityTick = 30 * time.Second

// csiCapacity is the latest capacity of the plugins, by plugin ID.
type csiCapacity struct {
	mu     sync.Mutex
	latest map[string][]*structs.CSIPluginCapacity
	polled map[string]time.Time
}

func newCSICapacity() *csiCapacity {
	return &csiCapacity{
		latest: map[string][]*structs.CSIPluginCapacity{},
		polled: map[string]time.Time{},
	}
}

// due reports whether the capacity of a plugin is to be asked at now.
func (c *csiCapacity) due(pluginID string, interval time.Duration, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.polled[pluginID]
	return !ok || now.Sub(last) >= interval
}

// set stores the answers of a poll of a plugin.
func (c *csiCapacity) set(pluginID string, caps []*structs.CSIPluginCapacity, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latest[pluginID] = caps
	c.polled[pluginID] = now
}

// forget drops a plugin: gone, or its capacity is off.
func (c *csiCapacity) forget(pluginID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.latest, pluginID)
	delete(c.polled, pluginID)
}

// keep drops every plugin but those of ids.
func (c *csiCapacity) keep(ids map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.latest {
		if !ids[id] {
			delete(c.latest, id)
			delete(c.polled, id)
		}
	}
	for id := range c.polled {
		if !ids[id] {
			delete(c.polled, id)
		}
	}
}

// get returns copies of the latest capacity of a plugin.
func (c *csiCapacity) get(pluginID string) []*structs.CSIPluginCapacity {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*structs.CSIPluginCapacity
	for _, cp := range c.latest[pluginID] {
		out = append(out, cp.Copy())
	}
	return out
}

// pluginSupportsGetCapacity reports whether a healthy controller of the plugin
// has the GET_CAPACITY capability.
func pluginSupportsGetCapacity(p *structs.CSIPlugin) bool {
	if p == nil {
		return false
	}
	for _, info := range p.Controllers {
		if info != nil && info.Healthy && info.ControllerInfo != nil && info.ControllerInfo.SupportsGetCapacity {
			return true
		}
	}
	return false
}

// pluginUsesTopologies reports whether the plugin has the
// VOLUME_ACCESSIBILITY_CONSTRAINTS capability: only then may a GetCapacity
// request name a topology.
func pluginUsesTopologies(p *structs.CSIPlugin) bool {
	for _, infos := range []map[string]*structs.CSIInfo{p.Controllers, p.Nodes} {
		for _, info := range infos {
			if info != nil && info.RequiresTopologies {
				return true
			}
		}
	}
	return false
}

// pluginNodeTopologies returns the distinct topology segments of the nodes of
// the plugin, sorted by their key.
func pluginNodeTopologies(p *structs.CSIPlugin) []*structs.CSITopology {
	byKey := map[string]*structs.CSITopology{}
	for _, info := range p.Nodes {
		if info == nil || info.NodeInfo == nil || info.NodeInfo.AccessibleTopology == nil {
			continue
		}
		t := info.NodeInfo.AccessibleTopology
		if key := structs.CSITopologyKey(t); key != "" {
			byKey[key] = t.Copy()
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*structs.CSITopology, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}

// pluginCapacityInterval returns the capacity_interval of the plugin, from the
// task of one of its controller allocations; the default when none is found.
func pluginCapacityInterval(snap *state.StateSnapshot, p *structs.CSIPlugin) time.Duration {
	for _, info := range p.Controllers {
		if info == nil || info.AllocID == "" {
			continue
		}
		alloc, err := snap.AllocByID(nil, info.AllocID)
		if err != nil || alloc == nil || alloc.Job == nil {
			continue
		}
		tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
		if tg == nil {
			continue
		}
		for _, task := range tg.Tasks {
			if cfg := task.CSIPluginConfig; cfg != nil && cfg.ID == p.ID {
				return cfg.CapacityInterval
			}
		}
	}
	return structs.DefaultCSICapacityInterval
}

// askPluginCapacity asks a controller of the plugin for its capacity, through
// a client that runs it. An error of the controller is in the answer.
func (s *Server) askPluginCapacity(pluginID string, req *cstructs.ClientCSIControllerGetCapacityRequest, now time.Time) *structs.CSIPluginCapacity {
	req.PluginID = pluginID
	out := &structs.CSIPluginCapacity{Topology: req.Topology.Copy(), At: now}
	var resp cstructs.ClientCSIControllerGetCapacityResponse
	if err := s.RPC("ClientCSI.ControllerGetCapacity", req, &resp); err != nil {
		out.Error = err.Error()
		return out
	}
	out.AvailableCapacity = resp.AvailableCapacity
	out.MaximumVolumeSize = resp.MaximumVolumeSize
	out.MinimumVolumeSize = resp.MinimumVolumeSize
	return out
}

// csiCapacityLoop asks the plugins for their capacity while this server is
// the leader.
func (s *Server) csiCapacityLoop(stopCh chan struct{}) {
	tick := time.NewTicker(csiCapacityTick)
	defer tick.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-s.shutdownCtx.Done():
			return
		case now := <-tick.C:
			if err := s.pollCSICapacity(now); err != nil {
				s.logger.Warn("CSI capacity: reading the plugins failed", "error", err)
			}
		}
	}
}

// pollCSICapacity asks every plugin whose capacity is due.
func (s *Server) pollCSICapacity(now time.Time) error {
	snap, err := s.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	iter, err := snap.CSIPlugins(nil)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var gauges []metricsstore.Gauge
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		p := raw.(*structs.CSIPlugin)
		if !pluginSupportsGetCapacity(p) {
			continue
		}
		interval := pluginCapacityInterval(snap, p)
		if interval <= 0 {
			continue
		}
		seen[p.ID] = true
		if !s.csiCapacity.due(p.ID, interval, now) {
			continue
		}
		caps := []*structs.CSIPluginCapacity{
			s.askPluginCapacity(p.ID, &cstructs.ClientCSIControllerGetCapacityRequest{}, now),
		}
		if pluginUsesTopologies(p) {
			for _, t := range pluginNodeTopologies(p) {
				caps = append(caps, s.askPluginCapacity(p.ID, &cstructs.ClientCSIControllerGetCapacityRequest{Topology: t}, now))
			}
		}
		s.csiCapacity.set(p.ID, caps, now)
		gauges = append(gauges, csiCapacityGauges(p.ID, caps)...)
	}
	s.csiCapacity.keep(seen)
	if s.metricsStore != nil && len(gauges) > 0 {
		s.metricsStore.RecordGauges(now, gauges)
	}
	return nil
}

// csiCapacityGauges are the gauges of the answers of a plugin; an answer with
// an error has none.
func csiCapacityGauges(pluginID string, caps []*structs.CSIPluginCapacity) []metricsstore.Gauge {
	var out []metricsstore.Gauge
	for _, c := range caps {
		if c == nil || c.Error != "" {
			continue
		}
		labels := map[string]string{"plugin": pluginID, "topology": c.TopologyKey()}
		out = append(out, metricsstore.Gauge{Metric: metricsstore.MetricCSICapacityAvailable, Labels: labels, Value: float64(c.AvailableCapacity)})
		if c.MaximumVolumeSize != nil {
			out = append(out, metricsstore.Gauge{Metric: metricsstore.MetricCSIMaximumVolumeSize, Labels: labels, Value: float64(*c.MaximumVolumeSize)})
		}
		if c.MinimumVolumeSize != nil {
			out = append(out, metricsstore.Gauge{Metric: metricsstore.MetricCSIMinimumVolumeSize, Labels: labels, Value: float64(*c.MinimumVolumeSize)})
		}
	}
	return out
}

// errNotEnoughCapacity is returned by volume create when the plugin reports
// less capacity than the volume asks for.
var errNotEnoughCapacity = errors.New("not enough capacity")

// checkCreateCapacity asks the plugin for the capacity for a volume to be
// created, with its parameters, capabilities and (for a plugin with
// topologies) its one required topology, and refuses the volume when the
// plugin reports less than capacity_min available or a largest volume size
// below it. A plugin without GET_CAPACITY, a volume without capacity_min, or
// a failed request let the create go on.
func (s *Server) checkCreateCapacity(vol *structs.CSIVolume, plugin *structs.CSIPlugin) error {
	if vol.RequestedCapacityMin <= 0 || !pluginSupportsGetCapacity(plugin) {
		return nil
	}
	req := &cstructs.ClientCSIControllerGetCapacityRequest{
		VolumeCapabilities: vol.RequestedCapabilities,
		MountOptions:       vol.MountOptions,
		Parameters:         vol.Parameters,
	}
	if pluginUsesTopologies(plugin) && vol.RequestedTopologies != nil && len(vol.RequestedTopologies.Required) == 1 {
		req.Topology = vol.RequestedTopologies.Required[0]
	}
	c := s.askPluginCapacity(plugin.ID, req, time.Now())
	if c.Error != "" {
		s.logger.Debug("CSI capacity: asking the plugin before create failed; creating the volume anyway",
			"plugin_id", plugin.ID, "volume", vol.ID, "error", c.Error)
		return nil
	}
	metrics.IncrCounter([]string{"nomad", "volume", "create_capacity_checked"}, 1)
	where := ""
	if key := c.TopologyKey(); key != "" {
		where = " in " + key
	}
	if c.AvailableCapacity < vol.RequestedCapacityMin {
		return fmt.Errorf("%w: plugin %q reports %d bytes available%s, the volume needs capacity_min %d bytes",
			errNotEnoughCapacity, plugin.ID, c.AvailableCapacity, where, vol.RequestedCapacityMin)
	}
	if c.MaximumVolumeSize != nil && *c.MaximumVolumeSize < vol.RequestedCapacityMin {
		return fmt.Errorf("%w: plugin %q creates volumes of at most %d bytes%s, the volume needs capacity_min %d bytes",
			errNotEnoughCapacity, plugin.ID, *c.MaximumVolumeSize, where, vol.RequestedCapacityMin)
	}
	return nil
}

// Capacity asks a controller of a plugin for the capacity of its storage for
// volumes with the capabilities, parameters and topology of the request
// (norca plugin capacity). Plugin read is required.
func (v *CSIPlugin) Capacity(args *structs.CSIPluginCapacityRequest, reply *structs.CSIPluginCapacityResponse) error {
	authErr := v.srv.Authenticate(v.ctx, args)
	if done, err := v.srv.forward("CSIPlugin.Capacity", args, args, reply); done {
		return err
	}
	v.srv.MeasureRPCRate("csi_plugin", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "plugin", "capacity"}, time.Now())

	aclObj, err := v.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowPluginRead() {
		return structs.ErrPermissionDenied
	}
	if args.ID == "" {
		return fmt.Errorf("missing plugin ID")
	}

	plugin, err := v.srv.fsm.State().CSIPluginByID(nil, args.ID)
	if err != nil {
		return err
	}
	if plugin == nil {
		return fmt.Errorf("plugin %q not found", args.ID)
	}
	if !pluginSupportsGetCapacity(plugin) {
		return fmt.Errorf("plugin %q has no healthy controller with the GET_CAPACITY capability", args.ID)
	}
	if args.Topology != nil && len(args.Topology.Segments) > 0 && !pluginUsesTopologies(plugin) {
		return fmt.Errorf("plugin %q has no topologies (VOLUME_ACCESSIBILITY_CONSTRAINTS); ask without a topology", args.ID)
	}

	reply.Capacity = v.srv.askPluginCapacity(plugin.ID, &cstructs.ClientCSIControllerGetCapacityRequest{
		VolumeCapabilities: args.Capabilities,
		MountOptions:       args.MountOptions,
		Parameters:         args.Parameters,
		Topology:           args.Topology,
	}, time.Now())
	if reply.Capacity.Error != "" {
		return errors.New(reply.Capacity.Error)
	}
	return nil
}
