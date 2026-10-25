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
