// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/helper/pointer"
)

// Vault compatibility of the client (norca-doc content/docs/design/secrets-plan.md, "Vault
// compatibility"): when the agent has no Vault integration, Vault-style
// lookups (the Vault "secret" template function and the reads of tasks with
// a vault block through the client's Vault-compatible endpoint) are served
// by the built-in store. The VaultKV rules map their Vault KV paths onto
// paths of the built-in store.

// VaultKVRule maps the Vault paths starting with Prefix onto the built-in
// store: the rest of the path after Prefix, appended to Path, is the path of
// the secret, and the value is returned in the response shape of the KV
// engine of KVVersion.
//
// A KV v2 rule whose prefix contains "/data/" also serves the metadata reads
// of the same paths ("<mount>/metadata/<rest>" for "<mount>/data/<rest>").
type VaultKVRule struct {
	// Prefix is the Vault path prefix, without a leading slash and ending
	// in "/", for example "kv/data/".
	Prefix string

	// KVVersion is 1 or 2; 0 is 2 for prefixes containing "/data/" and 1
	// otherwise.
	KVVersion int

	// Path is the prefix of the paths in the built-in store, "" or ending
	// in "/".
	Path string
}

// DefaultVaultKVRules are the rules used when the client configuration has
// none: "kv/data/<p>" and "secret/data/<p>" (KV v2) and "secret/<p>" (KV v1)
// read the built-in secret "<p>".
func DefaultVaultKVRules() []*VaultKVRule {
	return []*VaultKVRule{
		{Prefix: "kv/data/", KVVersion: 2},
		{Prefix: "secret/data/", KVVersion: 2},
		{Prefix: "secret/", KVVersion: 1},
	}
}

// Version returns the effective KV version of the rule.
func (r *VaultKVRule) Version() int {
	if r.KVVersion != 0 {
		return r.KVVersion
	}
	if strings.Contains(r.Prefix, "/data/") {
		return 2
	}
	return 1
}

// MetadataPrefix returns the prefix of the metadata reads the rule serves:
// "<mount>/metadata/<rest>" for a KV v2 rule of "<mount>/data/<rest>", ""
// otherwise.
func (r *VaultKVRule) MetadataPrefix() string {
	if r.Version() != 2 {
		return ""
	}
	if mount, rest, ok := strings.Cut(r.Prefix, "/data/"); ok && mount != "" {
		return mount + "/metadata/" + rest
	}
	return ""
}

// ValidateVaultKVRules checks the rules.
func ValidateVaultKVRules(rules []*VaultKVRule) error {
	seen := map[string]struct{}{}
	for _, r := range rules {
		if r == nil {
			continue
		}
		switch {
		case r.Prefix == "" || !strings.HasSuffix(r.Prefix, "/"):
			return fmt.Errorf("vault_kv %q: the prefix must end with \"/\"", r.Prefix)
		case strings.HasPrefix(r.Prefix, "/") || strings.Contains(r.Prefix, "//"):
			return fmt.Errorf("vault_kv %q: invalid prefix", r.Prefix)
		case r.KVVersion != 0 && r.KVVersion != 1 && r.KVVersion != 2:
			return fmt.Errorf("vault_kv %q: kv_version must be 1 or 2", r.Prefix)
		case r.Path != "" && (!strings.HasSuffix(r.Path, "/") || strings.HasPrefix(r.Path, "/") ||
			strings.Contains(r.Path, "//") || strings.Contains(r.Path, "..")):
			return fmt.Errorf("vault_kv %q: path must be empty or a relative prefix ending with \"/\"", r.Prefix)
		}
		if _, ok := seen[r.Prefix]; ok {
			return fmt.Errorf("duplicate vault_kv rule for prefix %q", r.Prefix)
		}
		seen[r.Prefix] = struct{}{}
	}
	return nil
}

// Defaults of the Vault-compatible endpoint.
const (
	DefaultVaultCompatPort      = 8250
	DefaultVaultCompatTokenTTL  = time.Hour
	DefaultVaultCompatRateLimit = 20.0
	DefaultVaultCompatRateBurst = 40
)

// VaultCompatConfig configures the Vault-compatible read endpoint of the
// client. It is only used when the agent has no Vault integration: tasks
// with a vault block then get a token of the endpoint instead of a Vault
// token.
type VaultCompatConfig struct {
	// Enabled enables the Vault compatibility of the client (the endpoint
	// and the Vault template functions served by the built-in store); nil
	// is enabled. Disabled, the client behaves as Nomad without Vault.
	Enabled *bool

	// Port is the TCP port of the endpoint, on 127.0.0.1 for tasks in the
	// host network and on the bridge gateway address for tasks in bridge
	// networking mode. 0 is DefaultVaultCompatPort.
	Port int

	// Bridge enables the listener on the bridge gateway address; nil is
	// enabled.
	Bridge *bool

	// TokenTTL is the time to live of a task token: the client renews it
	// while the task runs, a renew-self request extends it by up to the
	// TTL. 0 is DefaultVaultCompatTokenTTL.
	TokenTTL time.Duration

	// RateLimit and RateBurst limit the requests per second of each token
	// (and of the requests without a valid token, together).
	RateLimit float64
	RateBurst int
}

// Copy returns a copy of the configuration.
func (c *VaultCompatConfig) Copy() *VaultCompatConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.Enabled = pointer.Copy(c.Enabled)
	out.Bridge = pointer.Copy(c.Bridge)
	return &out
}

// IsEnabled reports whether the endpoint is enabled (it still only runs
// when the agent has no Vault integration).
func (c *VaultCompatConfig) IsEnabled() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

// BridgeEnabled reports whether the endpoint listens on the bridge gateway.
func (c *VaultCompatConfig) BridgeEnabled() bool {
	return c == nil || c.Bridge == nil || *c.Bridge
}

// PortOrDefault returns the effective port.
func (c *VaultCompatConfig) PortOrDefault() int {
	if c == nil || c.Port == 0 {
		return DefaultVaultCompatPort
	}
	return c.Port
}

// TokenTTLOrDefault returns the effective token TTL.
func (c *VaultCompatConfig) TokenTTLOrDefault() time.Duration {
	if c == nil || c.TokenTTL <= 0 {
		return DefaultVaultCompatTokenTTL
	}
	return c.TokenTTL
}

// Rate returns the effective rate limit and burst.
func (c *VaultCompatConfig) Rate() (float64, int) {
	limit, burst := DefaultVaultCompatRateLimit, DefaultVaultCompatRateBurst
	if c != nil && c.RateLimit > 0 {
		limit = c.RateLimit
	}
	if c != nil && c.RateBurst > 0 {
		burst = c.RateBurst
	}
	return limit, burst
}

// Validate checks the configuration.
func (c *VaultCompatConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("vault_compat port %d is out of range", c.Port)
	}
	if c.TokenTTL < 0 || (c.TokenTTL > 0 && c.TokenTTL < time.Minute) {
		return fmt.Errorf("vault_compat token_ttl must be at least 1m")
	}
	if c.RateLimit < 0 || c.RateBurst < 0 {
		return fmt.Errorf("vault_compat rate_limit and rate_burst must not be negative")
	}
	return nil
}

// VaultCompatEnabled reports whether the client serves the vault blocks of
// tasks from the built-in secret store: it has no Vault integration and the
// Vault-compatible endpoint is not disabled.
func (c *Config) VaultCompatEnabled() bool {
	if c.VaultConfig != nil && c.VaultConfig.IsEnabled() {
		return false
	}
	if c.Secrets == nil {
		return true
	}
	return c.Secrets.VaultCompat.IsEnabled()
}

// AttrVaultCompat is the node attribute of clients that serve the vault
// blocks of tasks from the built-in secret store.
const AttrVaultCompat = "norca.secrets.vault_compat"
