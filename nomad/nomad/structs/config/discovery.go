// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"fmt"
	"math"

	"github.com/hashicorp/nomad/helper/pointer"
)

// NativeServiceDiscoveryConfig is the native_service_discovery block of the
// client configuration.
type NativeServiceDiscoveryConfig struct {
	// DefaultHealthyOnly makes service lookups made by the workloads of the
	// client (templates and the Task API) return only healthy instances
	// unless the request says otherwise. Defaults to false.
	DefaultHealthyOnly *bool `hcl:"default_healthy_only"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a copy of c.
func (c *NativeServiceDiscoveryConfig) Copy() *NativeServiceDiscoveryConfig {
	if c == nil {
		return nil
	}
	return &NativeServiceDiscoveryConfig{DefaultHealthyOnly: pointer.Copy(c.DefaultHealthyOnly)}
}

// Merge returns c with the values set in o applied.
func (c *NativeServiceDiscoveryConfig) Merge(o *NativeServiceDiscoveryConfig) *NativeServiceDiscoveryConfig {
	switch {
	case c == nil:
		return o.Copy()
	case o == nil:
		return c.Copy()
	}
	result := c.Copy()
	if o.DefaultHealthyOnly != nil {
		result.DefaultHealthyOnly = pointer.Copy(o.DefaultHealthyOnly)
	}
	return result
}

// HealthyOnly returns the effective value of DefaultHealthyOnly.
func (c *NativeServiceDiscoveryConfig) HealthyOnly() bool {
	return c != nil && c.DefaultHealthyOnly != nil && *c.DefaultHealthyOnly
}

// ServerNativeServiceDiscoveryConfig is the native_service_discovery block of
// the server configuration.
type ServerNativeServiceDiscoveryConfig struct {
	// DefaultOverflowFraction is the overflow fraction the servers fill in at
	// job registration for discovery blocks that leave overflow_fraction
	// unset. It must be between 0 and 1; 0 disables the overflow rule. Unset
	// means 0.2. Each region's servers apply their own setting.
	DefaultOverflowFraction *float64 `hcl:"default_overflow_fraction"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a copy of c.
func (c *ServerNativeServiceDiscoveryConfig) Copy() *ServerNativeServiceDiscoveryConfig {
	if c == nil {
		return nil
	}
	return &ServerNativeServiceDiscoveryConfig{DefaultOverflowFraction: pointer.Copy(c.DefaultOverflowFraction)}
}

// Merge returns c with the values set in o applied.
func (c *ServerNativeServiceDiscoveryConfig) Merge(o *ServerNativeServiceDiscoveryConfig) *ServerNativeServiceDiscoveryConfig {
	switch {
	case c == nil:
		return o.Copy()
	case o == nil:
		return c.Copy()
	}
	result := c.Copy()
	if o.DefaultOverflowFraction != nil {
		result.DefaultOverflowFraction = pointer.Copy(o.DefaultOverflowFraction)
	}
	return result
}

// Validate checks the values of the block.
func (c *ServerNativeServiceDiscoveryConfig) Validate() error {
	if c == nil {
		return nil
	}
	if f := c.DefaultOverflowFraction; f != nil && (*f < 0 || *f > 1 || math.IsNaN(*f)) {
		return fmt.Errorf("default_overflow_fraction must be between 0 and 1; got %v", *f)
	}
	return nil
}
