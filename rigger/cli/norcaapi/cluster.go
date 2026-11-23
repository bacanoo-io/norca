// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca/rigger/host"
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
	// Not Agent().Self(), which takes no options: the request would not end
	// with ctx.
	var self api.AgentSelf
	if _, err := c.client.Raw().Query("/v1/agent/self", &self, query(ctx, "")); err != nil {
		return host.ClusterInfo{}, fmt.Errorf("the agent of the cluster: %w", err)
	}

	// NorcaVersion, not Config.Version: that is the version of the API
	// Norca is compatible with.
	info := host.ClusterInfo{Version: self.NorcaVersion}

	if region, ok := self.Config["Region"].(string); ok {
		info.Region = region
	}

	releases, err := c.hasReleases(ctx)
	if err != nil {
		return host.ClusterInfo{}, err
	}

	if releases {
		info.Features = append(info.Features, "releases")
	}

	return info, nil
}

// hasReleases reports whether the servers have the release API. An answer
// that does not tell (the servers fail, the request is cut) is an error: a
// cluster would look like one without releases, and a module that requires
// them would be refused for the wrong reason.
func (c *Cluster) hasReleases(ctx context.Context) (bool, error) {
	ok, err := c.client.Releases().Supported(query(ctx, ""))
	if err == nil {
		return ok, nil
	}

	// The servers answer for the API and refuse the token in the namespace
	// of the client: they have it.
	if uerr := (api.UnexpectedResponseError{}); errors.As(err, &uerr) && uerr.StatusCode() == http.StatusForbidden {
		return true, nil
	}

	return false, fmt.Errorf("whether the cluster has the release API: %w", err)
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
