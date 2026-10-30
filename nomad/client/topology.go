// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

// Topology returns the location of the node: its region and datacenter.
func (c *Client) Topology() structs.Topology {
	cfg := c.GetConfig()
	return structs.Topology{
		Region:     cfg.Region,
		Datacenter: cfg.Node.Datacenter,
	}
}
