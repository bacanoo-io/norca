// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"fmt"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/client/consulcompat"
	"github.com/hashicorp/nomad/client/discovery"
	"github.com/hashicorp/nomad/client/dns"
	"github.com/hashicorp/nomad/client/externalsvc"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/nats-io/nats.go"
)

// discoveryServers are the service discovery servers of the client: the
// built-in DNS server with the catalog cache it answers from, and the
// Consul-compatible API; and the agent of the external services owned by
// the node.
type discoveryServers struct {
	cache    *discovery.Cache
	dns      *dns.Server
	compat   *consulcompat.Server
	external *externalsvc.Agent
}

// setupDiscovery starts the service discovery servers enabled in the client
// configuration.
func (c *Client) setupDiscovery() error {
	cfg := c.GetConfig()

	if cfg.DNS.IsEnabled() {
		cache := discovery.NewCache(c.logger, c.RPC, c.catalogToken)
		resolver := discovery.NewResolver(cache, c.Topology)

		server, err := dns.New(c.logger, dns.Config{
			Bind:                     cfg.DNS.BindAddr(),
			Domain:                   cfg.DNS.DomainName(),
			ConsulCompat:             cfg.DNS.IsConsulCompat(),
			ConsulDatacenterIsRegion: cfg.ConsulCompat.DatacenterIsRegion(),
			Recursors:                cfg.DNS.RecursorAddrs(),
			TTL:                      cfg.DNS.TTLDuration(),
			Namespaces:               cfg.DNS.NamespaceList(),
		}, resolver)
		if err != nil {
			cache.Stop()
			return fmt.Errorf("failed to start the dns server: %w", err)
		}
		c.discovery.cache = cache
		c.discovery.dns = server
	}

	if cfg.ConsulCompat.IsEnabled() {
		compat := consulcompat.New(c.logger, consulcompat.Config{
			Bind:               cfg.ConsulCompat.BindAddr(),
			DatacenterIsRegion: cfg.ConsulCompat.DatacenterIsRegion(),
			Namespaces:         cfg.ConsulCompat.NamespaceList(),
			AnonymousToken:     cfg.ConsulCompat.AnonymousToken,
			KV:                 cfg.ConsulCompat.KV,
		}, compatBackend{c})
		if err := compat.Start(); err != nil {
			c.shutdownDiscovery()
			return fmt.Errorf("failed to start the consul-compatible API: %w", err)
		}
		c.discovery.compat = compat
	}

	// The external services owned by the node: their http and tcp checks
	// run here, and the Consul-compatible agent API serves them.
	c.discovery.external = externalsvc.New(c.logger, externalsvc.Config{
		RPC:        c.RPC,
		NodeID:     c.NodeID,
		NodeSecret: c.secretNodeID,
		Region:     c.Region,
	})
	c.discovery.external.Start()
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
	if c.discovery.compat != nil {
		c.discovery.compat.Shutdown()
	}
	if c.discovery.dns != nil {
		c.discovery.dns.Shutdown()
	}
	if c.discovery.cache != nil {
		c.discovery.cache.Stop()
	}
	if c.discovery.external != nil {
		c.discovery.external.Stop()
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

// ConsulCompatAddr returns the address of the Consul-compatible API, empty if
// it is not running.
func (c *Client) ConsulCompatAddr() string {
	if c.discovery.compat == nil {
		return ""
	}
	return c.discovery.compat.Addr()
}

// compatBackend gives the Consul-compatible API access to the client.
type compatBackend struct{ c *Client }

func (b compatBackend) RPC(method string, args, reply any) error  { return b.c.RPC(method, args, reply) }
func (b compatBackend) NodeSecret() string                        { return b.c.secretNodeID() }
func (b compatBackend) LocalNode() *structs.Node                  { return b.c.Node() }
func (b compatBackend) Region() string                            { return b.c.Region() }
func (b compatBackend) Topology() structs.Topology                { return b.c.Topology() }
func (b compatBackend) ResolveACL(token string) (*acl.ACL, error) { return b.c.ResolveToken(token) }
func (b compatBackend) BusConn() (*nats.Conn, error)              { return b.c.BusConn() }

// LocalServices returns the external services owned by the node.
func (b compatBackend) LocalServices() consulcompat.LocalServices {
	if b.c.discovery.external == nil {
		return nil
	}
	return b.c.discovery.external
}

// RegisterExternalService registers an external service owned by the node of
// the client: the node runs its http and tcp checks, and the registration is
// removed when the node goes down.
func (c *Client) RegisterExternalService(args *structs.ExternalServiceUpsertRequest,
	reply *structs.ExternalServiceUpsertResponse) error {

	args.NodeSecretID = c.secretNodeID()
	if err := c.RPC(structs.ServiceRegistrationUpsertExternalRPCMethod, args, reply); err != nil {
		return err
	}
	if c.discovery.external != nil {
		// start the checks right away
		if err := c.discovery.external.Refresh(); err != nil {
			c.logger.Debug("failed to refresh the external services of the node", "error", err)
		}
	}
	return nil
}
