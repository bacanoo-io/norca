// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"sort"
	"strings"
	"time"
)

// CSIPluginCapacity is the capacity of the storage of a CSI plugin, as one of
// its controllers answers GetCapacity (Norca, clean room, design
// csi-completion-plan.md, phase 3): for the whole storage (Topology nil) or
// for one topology segment of its nodes.
type CSIPluginCapacity struct {
	// Topology is the accessible topology of the request; nil for the whole
	// storage.
	Topology *CSITopology `json:",omitempty"`

	// AvailableCapacity is the bytes available for new volumes.
	AvailableCapacity int64

	// MaximumVolumeSize and MinimumVolumeSize are the largest and the
	// smallest size of a volume, when the plugin reports them.
	MaximumVolumeSize *int64 `json:",omitempty"`
	MinimumVolumeSize *int64 `json:",omitempty"`

	// Error is why the controller could not answer, if it could not.
	Error string `json:",omitempty"`

	// At is when the controller was asked.
	At time.Time
}

// Copy returns a copy of the capacity.
func (c *CSIPluginCapacity) Copy() *CSIPluginCapacity {
	if c == nil {
		return nil
	}
	out := *c
	out.Topology = c.Topology.Copy()
	if c.MaximumVolumeSize != nil {
		v := *c.MaximumVolumeSize
		out.MaximumVolumeSize = &v
	}
	if c.MinimumVolumeSize != nil {
		v := *c.MinimumVolumeSize
		out.MinimumVolumeSize = &v
	}
	return &out
}

// TopologyKey is the topology of a capacity as a label: the segments as
// sorted key=value pairs joined by commas, "" for the whole storage.
func (c *CSIPluginCapacity) TopologyKey() string {
	if c == nil {
		return ""
	}
	return CSITopologyKey(c.Topology)
}

// CSITopologyKey is a topology as sorted key=value pairs joined by commas.
func CSITopologyKey(t *CSITopology) string {
	if t == nil || len(t.Segments) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(t.Segments))
	for k, v := range t.Segments {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

const (
	// DefaultCSICapacityInterval is the default of csi_plugin
	// capacity_interval.
	DefaultCSICapacityInterval = 5 * time.Minute
)

// CSIPluginCapacityRequest asks a controller of a plugin for the capacity of
// its storage for volumes with the given capabilities, parameters and
// topology (norca plugin capacity). Every field but ID is optional.
type CSIPluginCapacityRequest struct {
	ID           string
	Capabilities []*CSIVolumeCapability
	MountOptions *CSIMountOptions
	Parameters   map[string]string
	Topology     *CSITopology
	QueryOptions
}

// CSIPluginCapacityResponse is the answer of the controller.
type CSIPluginCapacityResponse struct {
	Capacity *CSIPluginCapacity
	QueryMeta
}
