// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	clientconfig "github.com/hashicorp/nomad/client/config"
)

// ClientSecretsConfig is the client's secrets block: the routes that pick
// the provider of a secret lookup by its path when neither the secret block
// nor the template call names one (docs/design/secrets-plan.md, "Provider
// resolution and fallback", step 2). The longest matching prefix wins.
//
//	client {
//	  secrets {
//	    route "kv/" {
//	      provider = "vault"
//	    }
//	  }
//	}
//
// The label of a route block is its path prefix (labeled like the agent's
// other repeated blocks, which HCL1 decodes reliably).
//
// The providers known today are "norca" (the built-in store) and "vault"
// (the agent's Vault integration, when its vault block is enabled).
type ClientSecretsConfig struct {
	Routes []*ClientSecretsRoute `hcl:"route"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// ClientSecretsRoute is a route block.
type ClientSecretsRoute struct {
	Prefix   string `hcl:",key"`
	Provider string `hcl:"provider"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a deep copy of the block.
func (c *ClientSecretsConfig) Copy() *ClientSecretsConfig {
	if c == nil {
		return nil
	}
	out := &ClientSecretsConfig{}
	for _, r := range c.Routes {
		if r != nil {
			out.Routes = append(out.Routes, &ClientSecretsRoute{Prefix: r.Prefix, Provider: r.Provider})
		}
	}
	return out
}

// Merge merges two blocks: the routes of b replace the routes of c with the
// same prefix and are added otherwise.
func (c *ClientSecretsConfig) Merge(b *ClientSecretsConfig) *ClientSecretsConfig {
	if c == nil {
		return b.Copy()
	}
	result := c.Copy()
	if b == nil {
		return result
	}
	for _, r := range b.Copy().Routes {
		replaced := false
		for i, existing := range result.Routes {
			if existing.Prefix == r.Prefix {
				result.Routes[i] = r
				replaced = true
			}
		}
		if !replaced {
			result.Routes = append(result.Routes, r)
		}
	}
	return result
}

// toClientConfig converts the block to the client's configuration.
func (c *ClientSecretsConfig) toClientConfig() *clientconfig.SecretsConfig {
	if c == nil {
		return nil
	}
	out := &clientconfig.SecretsConfig{}
	for _, r := range c.Routes {
		if r != nil {
			out.Routes = append(out.Routes, &clientconfig.SecretsRoute{Prefix: r.Prefix, Provider: r.Provider})
		}
	}
	return out
}
