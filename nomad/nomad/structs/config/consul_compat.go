// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"fmt"
	"net"
	"slices"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper/pointer"
)

// Defaults of the consul_compat block of the client configuration.
const (
	DefaultConsulCompatBind = "127.0.0.1:8500"

	// ConsulCompatDatacenterFromRegion maps Consul datacenters to Nomad
	// regions.
	ConsulCompatDatacenterFromRegion = "region"

	// ConsulCompatDatacenterFromDatacenter maps Consul datacenters to Nomad
	// datacenters of the local region.
	ConsulCompatDatacenterFromDatacenter = "datacenter"
)

// ConsulCompatConfig is the consul_compat block of the client configuration:
// the read-only HTTP API compatible with the catalog and health endpoints of
// Consul, for tools that only know Consul.
type ConsulCompatConfig struct {
	// Enabled starts the API. Defaults to false.
	Enabled *bool `hcl:"enabled"`

	// Bind is the address the API listens on. Defaults to 127.0.0.1:8500.
	Bind string `hcl:"bind"`

	// DatacenterFrom is "region" (default) to show Nomad regions as Consul
	// datacenters, or "datacenter" to show the Nomad datacenters of the
	// local region.
	DatacenterFrom string `hcl:"datacenter_from"`

	// Namespaces are the Nomad namespaces shown. The services of the first
	// one appear under their name, those of the others as
	// "<namespace>--<name>". Defaults to ["default"].
	Namespaces []string `hcl:"namespaces"`

	// AnonymousToken is the Nomad ACL token used for requests that carry no
	// token. It should be a read-only token.
	AnonymousToken string `hcl:"anonymous_token"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a copy of c.
func (c *ConsulCompatConfig) Copy() *ConsulCompatConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Enabled = pointer.Copy(c.Enabled)
	nc.Namespaces = slices.Clone(c.Namespaces)
	nc.ExtraKeysHCL = nil
	return &nc
}

// Merge returns c with the values set in o applied.
func (c *ConsulCompatConfig) Merge(o *ConsulCompatConfig) *ConsulCompatConfig {
	switch {
	case c == nil:
		return o.Copy()
	case o == nil:
		return c.Copy()
	}
	r := c.Copy()
	if o.Enabled != nil {
		r.Enabled = pointer.Copy(o.Enabled)
	}
	if o.Bind != "" {
		r.Bind = o.Bind
	}
	if o.DatacenterFrom != "" {
		r.DatacenterFrom = o.DatacenterFrom
	}
	if len(o.Namespaces) > 0 {
		r.Namespaces = slices.Clone(o.Namespaces)
	}
	if o.AnonymousToken != "" {
		r.AnonymousToken = o.AnonymousToken
	}
	return r
}

// IsEnabled returns whether the API is enabled.
func (c *ConsulCompatConfig) IsEnabled() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

// BindAddr returns the effective bind address.
func (c *ConsulCompatConfig) BindAddr() string {
	if c == nil || c.Bind == "" {
		return DefaultConsulCompatBind
	}
	return c.Bind
}

// DatacenterIsRegion returns whether Consul datacenters are Nomad regions.
func (c *ConsulCompatConfig) DatacenterIsRegion() bool {
	return c == nil || c.DatacenterFrom == "" || c.DatacenterFrom == ConsulCompatDatacenterFromRegion
}

// NamespaceList returns the effective namespaces.
func (c *ConsulCompatConfig) NamespaceList() []string {
	if c == nil || len(c.Namespaces) == 0 {
		return []string{"default"}
	}
	return c.Namespaces
}

// Validate the consul_compat block.
func (c *ConsulCompatConfig) Validate() error {
	if c == nil {
		return nil
	}
	var mErr multierror.Error
	if _, _, err := net.SplitHostPort(c.BindAddr()); err != nil {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("consul_compat bind must be host:port: %v", err))
	}
	switch c.DatacenterFrom {
	case "", ConsulCompatDatacenterFromRegion, ConsulCompatDatacenterFromDatacenter:
	default:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("consul_compat datacenter_from must be %q or %q, got %q",
			ConsulCompatDatacenterFromRegion, ConsulCompatDatacenterFromDatacenter, c.DatacenterFrom))
	}
	for _, ns := range c.Namespaces {
		if ns == "" {
			mErr.Errors = append(mErr.Errors, errors.New("consul_compat namespaces must not be empty"))
		}
	}
	return mErr.ErrorOrNil()
}
