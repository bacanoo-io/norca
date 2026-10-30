// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"time"

	clientconfig "github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
)

// ClientSecretsConfig is the client's secrets block: the routes that pick
// the provider of a secret lookup by its path when neither the secret block
// nor the template call names one (norca-doc content/docs/design/secrets-plan.md, "Provider
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
//
// Without a Vault integration, Vault-style lookups (the Vault "secret"
// template function and tasks with a vault block) are served by the built-in
// store (norca-doc content/docs/design/secrets-plan.md, "Vault compatibility"). The vault_kv
// rules map their Vault paths onto the store, the vault_compat block
// configures the Vault-compatible read endpoint of tasks with a vault block:
//
//	client {
//	  secrets {
//	    # The label is the Vault path prefix. The rest of the path after it,
//	    # appended to path (default ""), is the secret read from the
//	    # built-in store; kv_version (1 or 2, default 2 for prefixes
//	    # containing "/data/", else 1) selects the response shape. A KV v2
//	    # rule of "<mount>/data/<p>" also serves "<mount>/metadata/<p>". The longest
//	    # matching prefix wins; configured rules replace the defaults
//	    # kv/data/ (v2), secret/data/ (v2) and secret/ (v1), all with path "".
//	    vault_kv "kv/data/" {
//	      kv_version = 2
//	      path       = "apps/"   # kv/data/db/pg reads the secret apps/db/pg
//	    }
//
//	    vault_compat {
//	      enabled    = true      # default; false keeps Nomad's behaviour without Vault
//	      port       = 8250      # on 127.0.0.1 and the bridge gateway address
//	      bridge     = true      # listen on the bridge gateway for bridge mode
//	      token_ttl  = "1h"      # renewed by the client while the task runs
//	      rate_limit = 20        # requests per second per token
//	      rate_burst = 40
//	    }
//	  }
//	}
type ClientSecretsConfig struct {
	Routes []*ClientSecretsRoute `hcl:"route"`

	// VaultKV are the rules mapping Vault KV paths to the built-in store.
	VaultKV []*ClientSecretsVaultKV `hcl:"vault_kv"`

	// VaultCompat configures the Vault-compatible endpoint.
	VaultCompat *ClientVaultCompatConfig `hcl:"vault_compat"`

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

// ClientSecretsVaultKV is a vault_kv block.
type ClientSecretsVaultKV struct {
	Prefix    string `hcl:",key"`
	KVVersion int    `hcl:"kv_version"`
	Path      string `hcl:"path"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// ClientVaultCompatConfig is the vault_compat block.
type ClientVaultCompatConfig struct {
	Enabled   *bool   `hcl:"enabled"`
	Port      int     `hcl:"port"`
	Bridge    *bool   `hcl:"bridge"`
	TokenTTL  string  `hcl:"token_ttl"`
	RateLimit float64 `hcl:"rate_limit"`
	RateBurst int     `hcl:"rate_burst"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a copy of the block.
func (c *ClientVaultCompatConfig) Copy() *ClientVaultCompatConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.Enabled = pointer.Copy(c.Enabled)
	out.Bridge = pointer.Copy(c.Bridge)
	out.ExtraKeysHCL = nil
	return &out
}

// Merge merges two blocks; the values set in b win.
func (c *ClientVaultCompatConfig) Merge(b *ClientVaultCompatConfig) *ClientVaultCompatConfig {
	switch {
	case c == nil:
		return b.Copy()
	case b == nil:
		return c.Copy()
	}
	r := c.Copy()
	if b.Enabled != nil {
		r.Enabled = pointer.Copy(b.Enabled)
	}
	if b.Port != 0 {
		r.Port = b.Port
	}
	if b.Bridge != nil {
		r.Bridge = pointer.Copy(b.Bridge)
	}
	if b.TokenTTL != "" {
		r.TokenTTL = b.TokenTTL
	}
	if b.RateLimit != 0 {
		r.RateLimit = b.RateLimit
	}
	if b.RateBurst != 0 {
		r.RateBurst = b.RateBurst
	}
	return r
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
	for _, r := range c.VaultKV {
		if r != nil {
			out.VaultKV = append(out.VaultKV, &ClientSecretsVaultKV{Prefix: r.Prefix, KVVersion: r.KVVersion, Path: r.Path})
		}
	}
	out.VaultCompat = c.VaultCompat.Copy()
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
	for _, r := range b.Copy().VaultKV {
		replaced := false
		for i, existing := range result.VaultKV {
			if existing.Prefix == r.Prefix {
				result.VaultKV[i] = r
				replaced = true
			}
		}
		if !replaced {
			result.VaultKV = append(result.VaultKV, r)
		}
	}
	result.VaultCompat = result.VaultCompat.Merge(b.VaultCompat)
	return result
}

// toClientConfig converts the block to the client's configuration.
func (c *ClientSecretsConfig) toClientConfig() (*clientconfig.SecretsConfig, error) {
	if c == nil {
		return nil, nil
	}
	out := &clientconfig.SecretsConfig{}
	for _, r := range c.Routes {
		if r != nil {
			out.Routes = append(out.Routes, &clientconfig.SecretsRoute{Prefix: r.Prefix, Provider: r.Provider})
		}
	}
	for _, r := range c.VaultKV {
		if r != nil {
			out.VaultKV = append(out.VaultKV, &clientconfig.VaultKVRule{Prefix: r.Prefix, KVVersion: r.KVVersion, Path: r.Path})
		}
	}
	if v := c.VaultCompat; v != nil {
		vc := &clientconfig.VaultCompatConfig{
			Enabled:   pointer.Copy(v.Enabled),
			Port:      v.Port,
			Bridge:    pointer.Copy(v.Bridge),
			RateLimit: v.RateLimit,
			RateBurst: v.RateBurst,
		}
		if v.TokenTTL != "" {
			ttl, err := time.ParseDuration(v.TokenTTL)
			if err != nil {
				return nil, fmt.Errorf("vault_compat token_ttl: %v", err)
			}
			vc.TokenTTL = ttl
		}
		out.VaultCompat = vc
	}
	return out, nil
}
