// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper/pointer"
)

// Defaults of the dns block of the client configuration.
const (
	DefaultDNSBind   = "127.0.0.1:8600"
	DefaultDNSDomain = "nomad"
	DefaultDNSTTL    = 5 * time.Second
)

// DNSConfig is the dns block of the client configuration: the built-in DNS
// server answering from the native service catalog.
type DNSConfig struct {
	// Enabled starts the DNS server. Defaults to false.
	Enabled *bool `hcl:"enabled"`

	// Bind is the address the DNS server listens on, over UDP and TCP.
	// Defaults to 127.0.0.1:8600.
	Bind string `hcl:"bind"`

	// Domain is the DNS domain of the native catalog. Defaults to "nomad".
	Domain string `hcl:"domain"`

	// ConsulCompat answers *.service.consul names as well.
	ConsulCompat *bool `hcl:"consul_compat"`

	// Recursors are the nameservers (host or host:port) queries outside the
	// domains of the server are forwarded to. Without recursors, such
	// queries are refused.
	Recursors []string `hcl:"recursors"`

	// TTL of the answers. Defaults to 5s.
	TTL string `hcl:"ttl"`

	// Namespaces are the namespaces whose services the DNS server answers
	// for. The first one is used for names without a namespace. Defaults to
	// ["default"].
	Namespaces []string `hcl:"namespaces"`

	// Token is the ACL token used to read the catalog. Defaults to the
	// identity of the node, which can read the services of every namespace;
	// the namespaces setting limits what is answered.
	Token string `hcl:"token"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a copy of c.
func (c *DNSConfig) Copy() *DNSConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Enabled = pointer.Copy(c.Enabled)
	nc.ConsulCompat = pointer.Copy(c.ConsulCompat)
	nc.Recursors = slices.Clone(c.Recursors)
	nc.Namespaces = slices.Clone(c.Namespaces)
	nc.ExtraKeysHCL = nil
	return &nc
}

// Merge returns c with the values set in o applied.
func (c *DNSConfig) Merge(o *DNSConfig) *DNSConfig {
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
	if o.Domain != "" {
		r.Domain = o.Domain
	}
	if o.ConsulCompat != nil {
		r.ConsulCompat = pointer.Copy(o.ConsulCompat)
	}
	if len(o.Recursors) > 0 {
		r.Recursors = slices.Clone(o.Recursors)
	}
	if o.TTL != "" {
		r.TTL = o.TTL
	}
	if len(o.Namespaces) > 0 {
		r.Namespaces = slices.Clone(o.Namespaces)
	}
	if o.Token != "" {
		r.Token = o.Token
	}
	return r
}

// IsEnabled returns whether the DNS server is enabled.
func (c *DNSConfig) IsEnabled() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

// IsConsulCompat returns whether *.service.consul names are answered.
func (c *DNSConfig) IsConsulCompat() bool {
	return c != nil && c.ConsulCompat != nil && *c.ConsulCompat
}

// BindAddr returns the effective bind address.
func (c *DNSConfig) BindAddr() string {
	if c == nil || c.Bind == "" {
		return DefaultDNSBind
	}
	return c.Bind
}

// DomainName returns the effective domain, without dots around it.
func (c *DNSConfig) DomainName() string {
	if c == nil || c.Domain == "" {
		return DefaultDNSDomain
	}
	return strings.Trim(strings.ToLower(c.Domain), ".")
}

// TTLDuration returns the effective TTL.
func (c *DNSConfig) TTLDuration() time.Duration {
	if c == nil || c.TTL == "" {
		return DefaultDNSTTL
	}
	d, err := time.ParseDuration(c.TTL)
	if err != nil {
		return DefaultDNSTTL
	}
	return d
}

// NamespaceList returns the effective namespaces.
func (c *DNSConfig) NamespaceList() []string {
	if c == nil || len(c.Namespaces) == 0 {
		return []string{"default"}
	}
	return c.Namespaces
}

// RecursorAddrs returns the recursors as host:port addresses.
func (c *DNSConfig) RecursorAddrs() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Recursors))
	for _, r := range c.Recursors {
		if _, _, err := net.SplitHostPort(r); err != nil {
			r = net.JoinHostPort(strings.Trim(r, "[]"), "53")
		}
		out = append(out, r)
	}
	return out
}

// Validate the dns block.
func (c *DNSConfig) Validate() error {
	if c == nil {
		return nil
	}
	var mErr multierror.Error
	if _, _, err := net.SplitHostPort(c.BindAddr()); err != nil {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("dns bind must be host:port: %v", err))
	}
	if c.TTL != "" {
		if d, err := time.ParseDuration(c.TTL); err != nil || d < 0 {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("dns ttl must be a non-negative duration, got %q", c.TTL))
		}
	}
	domain := c.DomainName()
	if domain == "" || strings.Contains(domain, "..") {
		mErr.Errors = append(mErr.Errors, errors.New("dns domain is invalid"))
	}
	if c.IsConsulCompat() && domain == "consul" {
		mErr.Errors = append(mErr.Errors, errors.New(`dns consul_compat cannot be used with domain "consul"`))
	}
	for _, ns := range c.Namespaces {
		if ns == "" {
			mErr.Errors = append(mErr.Errors, errors.New("dns namespaces must not be empty"))
		}
	}
	return mErr.ErrorOrNil()
}
