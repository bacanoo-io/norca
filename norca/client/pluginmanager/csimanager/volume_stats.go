// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csimanager

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/csi"
)

// Volume statistics and condition (Norca, clean room, design
// csi-completion-plan.md, phase 2; CSI spec v1.7.0 NodeGetVolumeStats).
//
// A node plugin with the GET_VOLUME_STATS node capability is asked for the
// usage of each volume it has published on the node, every stats_interval of
// the plugin task (csi_plugin { stats_interval }, 1m by default, 0 off). The
// latest answers are kept here; the client sends them with its usage report.

// volumeStatsTimeout bounds one NodeGetVolumeStats call.
const volumeStatsTimeout = 10 * time.Second

// volumeRef is what the stats of a published volume need besides its ID.
type volumeRef struct {
	namespace string
	remoteID  string
}

// publishedVolume is a volume published on the node under one usage, with an
// allocation that uses it (its target path is under the allocation).
type publishedVolume struct {
	key     volumeUsageKey
	ref     volumeRef
	allocID string
}

// published returns the volumes the tracker has claims for, in a stable order.
func (v *volumeUsageTracker) published() []publishedVolume {
	v.stateMu.Lock()
	defer v.stateMu.Unlock()
	out := make([]publishedVolume, 0, len(v.state))
	for key, allocs := range v.state {
		if len(allocs) == 0 {
			continue
		}
		out = append(out, publishedVolume{key: key, ref: v.refs[key], allocID: allocs[0]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.id != out[j].key.id {
			return out[i].key.id < out[j].key.id
		}
		return out[i].key.usageOpts.ToFS() < out[j].key.usageOpts.ToFS()
	})
	return out
}

// setRef records the namespace and the external ID of a claimed volume.
func (v *volumeUsageTracker) setRef(volID string, usage *UsageOptions, ref volumeRef) {
	v.stateMu.Lock()
	defer v.stateMu.Unlock()
	key := volumeUsageKey{id: volID, usageOpts: *usage}
	if _, ok := v.state[key]; ok {
		v.refs[key] = ref
	}
}

// collectStats asks the plugin for the usage of every volume published on the
// node. A volume published under more than one usage is reported once: an
// abnormal answer wins over a normal one, an answer over an error.
func (v *volumeManager) collectStats(ctx context.Context, pluginID string, now time.Time) []*structs.CSIVolumeNodeUsage {
	byVol := map[[2]string]*structs.CSIVolumeNodeUsage{}
	var order [][2]string
	for _, pv := range v.usageTracker.published() {
		usage := pv.key.usageOpts
		req := &csi.NodeGetVolumeStatsRequest{
			ExternalVolumeID: pv.ref.remoteID,
			VolumePath:       v.targetForVolume(v.containerMountPoint, pv.key.id, pv.allocID, &usage),
		}
		if req.ExternalVolumeID == "" {
			req.ExternalVolumeID = pv.key.id
		}
		if v.requiresStaging {
			req.StagingPath = v.stagingDirForVolume(v.containerMountPoint, pv.key.id, &usage)
		}

		callCtx, cancel := context.WithTimeout(ctx, volumeStatsTimeout)
		resp, err := v.plugin.NodeGetVolumeStats(callCtx, req)
		cancel()
		if err != nil && errors.Is(err, structs.ErrCSIClientRPCIgnorable) {
			// Unpublished meanwhile: nothing to report.
			continue
		}

		u := volumeUsageFromStats(resp, err)
		u.Namespace = pv.ref.namespace
		u.VolumeID = pv.key.id
		u.PluginID = pluginID
		u.At = now

		id := [2]string{u.Namespace, u.VolumeID}
		prev, ok := byVol[id]
		if !ok {
			byVol[id] = u
			order = append(order, id)
			continue
		}
		if (prev.Error != "" && u.Error == "") || (u.Abnormal && !prev.Abnormal) {
			byVol[id] = u
		}
	}
	out := make([]*structs.CSIVolumeNodeUsage, 0, len(order))
	for _, id := range order {
		out = append(out, byVol[id])
	}
	return out
}

// volumeUsageFromStats converts a NodeGetVolumeStats answer.
func volumeUsageFromStats(resp *csi.NodeGetVolumeStatsResponse, err error) *structs.CSIVolumeNodeUsage {
	u := &structs.CSIVolumeNodeUsage{}
	if err != nil {
		u.Error = err.Error()
		return u
	}
	if resp == nil {
		return u
	}
	if b := resp.Bytes; b != nil {
		u.BytesKnown = true
		u.BytesUsed, u.BytesAvailable, u.BytesTotal = b.Used, b.Available, b.Total
	}
	if n := resp.Inodes; n != nil {
		u.InodesKnown = true
		u.InodesUsed, u.InodesFree, u.InodesTotal = n.Used, n.Available, n.Total
	}
	if c := resp.Condition; c != nil {
		u.ConditionKnown = true
		u.Abnormal = c.Abnormal
		u.Message = c.Message
	}
	return u
}

// volumeStatsCache holds the latest stats of a plugin instance.
type volumeStatsCache struct {
	mu     sync.Mutex
	latest []*structs.CSIVolumeNodeUsage
}

func (c *volumeStatsCache) set(stats []*structs.CSIVolumeNodeUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latest = stats
}

func (c *volumeStatsCache) get() []*structs.CSIVolumeNodeUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*structs.CSIVolumeNodeUsage, 0, len(c.latest))
	for _, u := range c.latest {
		out = append(out, u.Copy())
	}
	return out
}

// statsInterval is the stats interval of the plugin task, from the plugin
// registration; the default when it is missing or invalid, 0 when off.
func (i *instanceManager) statsInterval() time.Duration {
	s, ok := i.info.Options["StatsInterval"]
	if !ok || s == "" {
		return structs.DefaultCSIVolumeStatsInterval
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return structs.DefaultCSIVolumeStatsInterval
	}
	return d
}

// runStatsLoop collects the volume stats of a node plugin until shutdown.
func (i *instanceManager) runStatsLoop() {
	interval := i.statsInterval()
	if interval <= 0 {
		return
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-i.shutdownCtx.Done():
			return
		case <-timer.C:
		}
		if caps := i.fp.nodeCaps.Load(); caps != nil && caps.HasGetVolumeStats {
			i.stats.set(i.volumeManager.collectStats(i.shutdownCtx, i.info.Name, time.Now()))
		} else {
			i.stats.set(nil)
		}
		timer.Reset(interval)
	}
}

// VolumeStats returns the latest stats of the volumes published by the node
// plugins of the node.
func (c *csiManager) VolumeStats() []*structs.CSIVolumeNodeUsage {
	c.instancesLock.RLock()
	defer c.instancesLock.RUnlock()
	var out []*structs.CSIVolumeNodeUsage
	names := make([]string, 0, len(c.instances["csi-node"]))
	for name := range c.instances["csi-node"] {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, c.instances["csi-node"][name].stats.get()...)
	}
	return out
}
