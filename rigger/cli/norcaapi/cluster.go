// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca-rig/host"
)

// Cluster is a Norca cluster (host.Cluster).
type Cluster struct {
	client *api.Client
}

var _ host.Cluster = (*Cluster)(nil)

// NewCluster returns the cluster of client.
func NewCluster(client *api.Client) *Cluster {
	return &Cluster{client: client}
}

// Info returns the version and the region of the agent the client talks
// to, and the release API as a feature when the servers have it.
func (c *Cluster) Info(ctx context.Context) (host.ClusterInfo, error) {
	self, err := c.client.Agent().Self()
	if err != nil {
		return host.ClusterInfo{}, err
	}

	info := host.ClusterInfo{}

	if cfg, ok := self.Config["Version"].(map[string]any); ok {
		info.Version, _ = cfg["Version"].(string)
	}

	if region, ok := self.Config["Region"].(string); ok {
		info.Region = region
	}

	if ok, err := c.client.Releases().Supported(query(ctx, "")); err == nil && ok {
		info.Features = append(info.Features, "releases")
	}

	return info, nil
}

// Namespaces returns the names of the namespaces of the region.
func (c *Cluster) Namespaces(ctx context.Context) ([]string, error) {
	nss, _, err := c.client.Namespaces().List(query(ctx, ""))
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(nss))
	for _, ns := range nss {
		out = append(out, ns.Name)
	}

	return out, nil
}
