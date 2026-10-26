// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"fmt"
	"regexp"
)

// SecretsConfig is the client's secrets configuration (the agent's
// client { secrets { } } block): which provider answers a secret lookup when
// neither the secret block nor a template call names one
// (docs/design/secrets-plan.md, "Provider resolution and fallback", step 2).
// External provider blocks are added by a later phase.
type SecretsConfig struct {
	// Routes map secret path prefixes to providers. The longest matching
	// prefix wins.
	Routes []*SecretsRoute

	// VaultKV are the rules that map the paths of Vault-style lookups to
	// the built-in store when no Vault is configured; nil applies
	// DefaultVaultKVRules.
	VaultKV []*VaultKVRule

	// VaultCompat configures the Vault-compatible read endpoint of the
	// client for tasks with a vault block when no Vault is configured.
	VaultCompat *VaultCompatConfig
}

// SecretsRoute routes the lookups of the paths starting with Prefix to the
// named provider.
type SecretsRoute struct {
	Prefix   string
	Provider string
}

var validSecretsProviderName = regexp.MustCompile("^[a-zA-Z0-9_-]{1,64}$")

// Copy returns a deep copy of the configuration.
func (c *SecretsConfig) Copy() *SecretsConfig {
	if c == nil {
		return nil
	}
	out := &SecretsConfig{}
	for _, r := range c.Routes {
		if r != nil {
			rr := *r
			out.Routes = append(out.Routes, &rr)
		}
	}
	for _, r := range c.VaultKV {
		if r != nil {
			rr := *r
			out.VaultKV = append(out.VaultKV, &rr)
		}
	}
	out.VaultCompat = c.VaultCompat.Copy()
	return out
}

// Validate checks the routes; known are the providers configured on the
// client.
func (c *SecretsConfig) Validate(known func(string) bool) error {
	if c == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, r := range c.Routes {
		if r == nil {
			continue
		}
		if r.Prefix == "" {
			return fmt.Errorf("secrets route requires a prefix")
		}
		if _, ok := seen[r.Prefix]; ok {
			return fmt.Errorf("duplicate secrets route for prefix %q", r.Prefix)
		}
		seen[r.Prefix] = struct{}{}
		if !validSecretsProviderName.MatchString(r.Provider) {
			return fmt.Errorf("secrets route %q: invalid provider name %q", r.Prefix, r.Provider)
		}
		if known != nil && !known(r.Provider) {
			return fmt.Errorf("secrets route %q: provider %q is not configured on this client", r.Prefix, r.Provider)
		}
	}
	if err := ValidateVaultKVRules(c.VaultKV); err != nil {
		return err
	}
	return c.VaultCompat.Validate()
}
