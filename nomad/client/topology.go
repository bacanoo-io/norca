// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// TopologyZone returns the topology zone of the node, as set by the topology
// block of the client configuration (by default the node meta "zone"). It is
// empty if the node has no zone.
func (c *Client) TopologyZone() string {
	cfg := c.GetConfig()
	return resolveTopologyZone(cfg.Topology.ZoneSource(), cfg.Node)
}

// resolveTopologyZone resolves source, which is either a literal zone or a
// reference to a node attribute or meta value ("${attr.<name>}" or
// "${meta.<name>}"), against node.
func resolveTopologyZone(source string, node *structs.Node) string {
	if !strings.HasPrefix(source, "${") || !strings.HasSuffix(source, "}") {
		return source
	}
	if node == nil {
		return ""
	}
	ref := strings.TrimSuffix(strings.TrimPrefix(source, "${"), "}")
	switch {
	case strings.HasPrefix(ref, "attr."):
		return node.Attributes[strings.TrimPrefix(ref, "attr.")]
	case strings.HasPrefix(ref, "meta."):
		return node.Meta[strings.TrimPrefix(ref, "meta.")]
	case ref == "node.datacenter":
		return node.Datacenter
	}
	return ""
}

// Topology returns the location of the node: its region, datacenter and zone.
func (c *Client) Topology() structs.Topology {
	cfg := c.GetConfig()
	return structs.Topology{
		Region:     cfg.Region,
		Datacenter: cfg.Node.Datacenter,
		Zone:       resolveTopologyZone(cfg.Topology.ZoneSource(), cfg.Node),
	}
}
