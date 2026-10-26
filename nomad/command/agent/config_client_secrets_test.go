// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/ci"
	clientconfig "github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/shoenig/test/must"
)

func TestConfig_ClientSecrets(t *testing.T) {
	ci.Parallel(t)

	path := filepath.Join(t.TempDir(), "client.hcl")
	must.NoError(t, os.WriteFile(path, []byte(`
client {
  enabled = true
  secrets {
    route "kv/" {
      provider = "vault"
    }
    route "app/" {
      provider = "norca"
    }
  }
}
`), 0o600))
	c, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.Eq(t, &ClientSecretsConfig{Routes: []*ClientSecretsRoute{
		{Prefix: "kv/", Provider: "vault"},
		{Prefix: "app/", Provider: "norca"},
	}}, c.Client.Secrets.Copy())

	// Merging replaces routes of the same prefix.
	merged := c.Client.Secrets.Merge(&ClientSecretsConfig{Routes: []*ClientSecretsRoute{
		{Prefix: "kv/", Provider: "norca"}, {Prefix: "x/", Provider: "norca"},
	}})
	must.Eq(t, []*ClientSecretsRoute{
		{Prefix: "kv/", Provider: "norca"}, {Prefix: "app/", Provider: "norca"}, {Prefix: "x/", Provider: "norca"},
	}, merged.Routes)

	// The client configuration rejects routes to providers it doesn't have:
	// the vault provider needs the agent's vault block.
	conf := DevConfig(nil)
	conf.Client.Secrets = c.Client.Secrets
	conf.Vault.Enabled = pointer.Of(false)
	_, err = convertClientConfig(conf)
	must.ErrorContains(t, err, `provider "vault" is not configured`)

	conf.Vault.Enabled = pointer.Of(true)
	cc, err := convertClientConfig(conf)
	must.NoError(t, err)
	must.Eq(t, &clientconfig.SecretsConfig{Routes: []*clientconfig.SecretsRoute{
		{Prefix: "kv/", Provider: "vault"}, {Prefix: "app/", Provider: "norca"},
	}}, cc.Secrets)
}

func TestConfig_ClientSecretsVaultCompat(t *testing.T) {
	ci.Parallel(t)

	path := filepath.Join(t.TempDir(), "client.hcl")
	must.NoError(t, os.WriteFile(path, []byte(`
client {
  enabled = true
  secrets {
    vault_kv "kv/data/" {
      path = "apps/"
    }
    vault_kv "legacy/" {
      kv_version = 1
    }
    vault_compat {
      port       = 8300
      bridge     = false
      token_ttl  = "30m"
      rate_limit = 5
      rate_burst = 10
    }
  }
}
server {
  secrets {
    vault_fallback = false
  }
}
`), 0o600))
	c, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.Eq(t, []*ClientSecretsVaultKV{
		{Prefix: "kv/data/", Path: "apps/"}, {Prefix: "legacy/", KVVersion: 1},
	}, c.Client.Secrets.Copy().VaultKV)
	must.False(t, c.Server.Secrets.vaultFallback())

	conf := DevConfig(nil)
	conf.Client.Secrets = c.Client.Secrets
	conf.Vault.Enabled = pointer.Of(false)
	cc, err := convertClientConfig(conf)
	must.NoError(t, err)
	must.Eq(t, []*clientconfig.VaultKVRule{
		{Prefix: "kv/data/", Path: "apps/"}, {Prefix: "legacy/", KVVersion: 1},
	}, cc.Secrets.VaultKV)
	compat := cc.Secrets.VaultCompat
	must.True(t, compat.IsEnabled())
	must.False(t, compat.BridgeEnabled())
	must.Eq(t, 8300, compat.PortOrDefault())
	must.Eq(t, "30m0s", compat.TokenTTLOrDefault().String())
	limit, burst := compat.Rate()
	must.Eq(t, 5.0, limit)
	must.Eq(t, 10, burst)
	must.True(t, cc.VaultCompatEnabled())

	// With a Vault integration the endpoint is not used.
	conf.Vault.Enabled = pointer.Of(true)
	cc, err = convertClientConfig(conf)
	must.NoError(t, err)
	must.False(t, cc.VaultCompatEnabled())

	// Merging: a later block replaces a rule of the same prefix and sets
	// the fields it has.
	merged := c.Client.Secrets.Merge(&ClientSecretsConfig{
		VaultKV:     []*ClientSecretsVaultKV{{Prefix: "legacy/", KVVersion: 2}},
		VaultCompat: &ClientVaultCompatConfig{Enabled: pointer.Of(false)},
	})
	must.Eq(t, 2, merged.VaultKV[1].KVVersion)
	must.False(t, *merged.VaultCompat.Enabled)
	must.Eq(t, 8300, merged.VaultCompat.Port)

	// Invalid rules and settings fail at agent start.
	for _, bad := range []string{
		`vault_kv "kv/data" {}`,
		`vault_kv "kv/" { kv_version = 3 }`,
		`vault_compat { token_ttl = "10s" }`,
		`vault_compat { token_ttl = "soon" }`,
		`vault_compat { port = 70000 }`,
	} {
		path := filepath.Join(t.TempDir(), "bad.hcl")
		must.NoError(t, os.WriteFile(path, []byte(`client { secrets { `+bad+` } }`), 0o600))
		c, err := ParseConfigFile(path)
		must.NoError(t, err, must.Sprint(bad))
		conf := DevConfig(nil)
		conf.Client.Secrets = c.Client.Secrets
		_, err = convertClientConfig(conf)
		must.Error(t, err, must.Sprint(bad))
	}
}
