// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"fmt"
	"time"

	"github.com/hashicorp/nomad/helper/pointer"
)

const (
	// DefaultMTLSLeafCertTTL is the default lifetime of workload
	// certificates.
	DefaultMTLSLeafCertTTL = 24 * time.Hour

	// DefaultMTLSCACertTTL is the default lifetime of the CA certificate of a
	// region (5 years). The leader rotates the CA when two thirds of it have
	// passed.
	DefaultMTLSCACertTTL = 5 * 365 * 24 * time.Hour

	// minMTLSLeafCertTTL bounds the lifetime of workload certificates from
	// below, so renewals stay cheap.
	minMTLSLeafCertTTL = 10 * time.Minute
)

// MTLSConfig is the mtls block of the server configuration: the native mTLS
// certificate authority of the region.
type MTLSConfig struct {
	// Enabled runs the certificate authority. Defaults to true; the CA is
	// only used by jobs with an mtls block.
	Enabled *bool `hcl:"enabled"`

	// TrustDomain is the SPIFFE trust domain of the region. Defaults to
	// "<authoritative region>.nomad", so the regions of a federation share
	// a trust domain.
	TrustDomain string `hcl:"trust_domain"`

	// LeafCertTTL is the lifetime of workload certificates (default 24h).
	LeafCertTTL    time.Duration `hcl:"-"`
	LeafCertTTLHCL string        `hcl:"leaf_cert_ttl" json:"-"`

	// CACertTTL is the lifetime of the CA certificate (default 5 years).
	CACertTTL    time.Duration `hcl:"-"`
	CACertTTLHCL string        `hcl:"ca_cert_ttl" json:"-"`

	// Federation copies the CAs of the other regions into the trust bundle
	// of the region. Defaults to true.
	Federation *bool `hcl:"federation"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// DefaultMTLSConfig returns the default mtls configuration.
func DefaultMTLSConfig() *MTLSConfig {
	c := &MTLSConfig{}
	_ = c.Finalize()
	return c
}

// Copy returns a copy of c.
func (c *MTLSConfig) Copy() *MTLSConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Enabled = pointer.Copy(c.Enabled)
	nc.Federation = pointer.Copy(c.Federation)
	nc.ExtraKeysHCL = nil
	return &nc
}

// Merge returns c with the values set in o applied.
func (c *MTLSConfig) Merge(o *MTLSConfig) *MTLSConfig {
	switch {
	case c == nil:
		return o.Copy()
	case o == nil:
		return c.Copy()
	}
	result := c.Copy()
	if o.Enabled != nil {
		result.Enabled = pointer.Copy(o.Enabled)
	}
	if o.TrustDomain != "" {
		result.TrustDomain = o.TrustDomain
	}
	if o.LeafCertTTL != 0 {
		result.LeafCertTTL = o.LeafCertTTL
	}
	if o.LeafCertTTLHCL != "" {
		result.LeafCertTTLHCL = o.LeafCertTTLHCL
	}
	if o.CACertTTL != 0 {
		result.CACertTTL = o.CACertTTL
	}
	if o.CACertTTLHCL != "" {
		result.CACertTTLHCL = o.CACertTTLHCL
	}
	if o.Federation != nil {
		result.Federation = pointer.Copy(o.Federation)
	}
	return result
}

// Finalize parses the durations, sets the defaults and validates the
// configuration. The trust domain default depends on the region and is set
// by the server.
func (c *MTLSConfig) Finalize() error {
	if c.Enabled == nil {
		c.Enabled = pointer.Of(true)
	}
	if c.Federation == nil {
		c.Federation = pointer.Of(true)
	}
	if c.LeafCertTTLHCL != "" {
		d, err := time.ParseDuration(c.LeafCertTTLHCL)
		if err != nil {
			return fmt.Errorf("invalid leaf_cert_ttl: %w", err)
		}
		c.LeafCertTTL = d
	}
	if c.CACertTTLHCL != "" {
		d, err := time.ParseDuration(c.CACertTTLHCL)
		if err != nil {
			return fmt.Errorf("invalid ca_cert_ttl: %w", err)
		}
		c.CACertTTL = d
	}
	if c.LeafCertTTL == 0 {
		c.LeafCertTTL = DefaultMTLSLeafCertTTL
	}
	if c.CACertTTL == 0 {
		c.CACertTTL = DefaultMTLSCACertTTL
	}
	if c.LeafCertTTL < minMTLSLeafCertTTL {
		return fmt.Errorf("leaf_cert_ttl must be at least %v", minMTLSLeafCertTTL)
	}
	// The CA must outlive several leaf certificates, so retired CAs can stay
	// in the trust bundle until their certificates expire.
	if c.CACertTTL < 10*c.LeafCertTTL {
		return fmt.Errorf("ca_cert_ttl must be at least ten times leaf_cert_ttl")
	}
	return nil
}

// IsEnabled returns whether the certificate authority runs.
func (c *MTLSConfig) IsEnabled() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

// FederationEnabled returns whether the CAs of other regions are trusted.
func (c *MTLSConfig) FederationEnabled() bool {
	return c == nil || c.Federation == nil || *c.Federation
}
