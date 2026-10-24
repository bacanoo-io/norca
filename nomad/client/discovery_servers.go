// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"fmt"

	"github.com/hashicorp/nomad/client/discovery"
	"github.com/hashicorp/nomad/client/dns"
)

// discoveryServers are the service discovery servers of the client: the
// built-in DNS server, and the catalog cache it answers from.
type discoveryServers struct {
	cache *discovery.Cache
	dns   *dns.Server
}

// setupDiscovery starts the service discovery servers enabled in the client
// configuration.
func (c *Client) setupDiscovery() error {
	cfg := c.GetConfig()
	if !cfg.DNS.IsEnabled() {
		return nil
	}

	cache := discovery.NewCache(c.logger, c.RPC, c.catalogToken)
	resolver := discovery.NewResolver(cache, c.Topology)

	server, err := dns.New(c.logger, dns.Config{
		Bind:                     cfg.DNS.BindAddr(),
		Domain:                   cfg.DNS.DomainName(),
		ConsulCompat:             cfg.DNS.IsConsulCompat(),
		ConsulDatacenterIsRegion: true,
		Recursors:                cfg.DNS.RecursorAddrs(),
		TTL:                      cfg.DNS.TTLDuration(),
		Namespaces:               cfg.DNS.NamespaceList(),
	}, resolver)
	if err != nil {
		cache.Stop()
		return fmt.Errorf("failed to start the dns server: %w", err)
	}

	c.discovery = discoveryServers{cache: cache, dns: server}
	return nil
}

// catalogToken returns the ACL token the catalog cache reads with: the token
// of the dns block, or the identity of the node.
func (c *Client) catalogToken() string {
	if token := c.GetConfig().DNS; token != nil && token.Token != "" {
		return token.Token
	}
	return c.secretNodeID()
}

// shutdownDiscovery stops the service discovery servers.
func (c *Client) shutdownDiscovery() {
	if c.discovery.dns != nil {
		c.discovery.dns.Shutdown()
	}
	if c.discovery.cache != nil {
		c.discovery.cache.Stop()
	}
}

// DNSAddr returns the address of the built-in DNS server, empty if it is not
// running.
func (c *Client) DNSAddr() string {
	if c.discovery.dns == nil {
		return ""
	}
	return c.discovery.dns.Addr()
}
