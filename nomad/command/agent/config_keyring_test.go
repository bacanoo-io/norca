// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

func TestConfig_ParseServerKeyring(t *testing.T) {
	ci.Parallel(t)

	c, err := parseTestConfig(t, `
server {
  keyring {
    seal "shares" {
      threshold = 2
      shares    = 4
    }
  }
}
`)
	must.NoError(t, err)
	merged := DevConfig(nil).Merge(c)
	must.NoError(t, merged.normalizeAddrs())
	serverConf, err := convertServerConfig(merged)
	must.NoError(t, err)
	seal := serverConf.KeyringConfig.Seal()
	must.Eq(t, config.KeyringSealShares, seal.Type)
	must.Eq(t, 2, seal.Threshold)
	must.Eq(t, 4, seal.Shares)

	// Without a keyring block: aead.
	merged = DevConfig(nil)
	must.NoError(t, merged.normalizeAddrs())
	serverConf, err = convertServerConfig(merged)
	must.NoError(t, err)
	must.Nil(t, serverConf.KeyringConfig)
	must.Eq(t, config.KeyringSealAEAD, serverConf.KeyringConfig.Seal().Type)

	_, err = parseTestConfig(t, `
server {
  keyring {
    seal "shares" {
      treshold = 2
    }
  }
}
`)
	must.ErrorContains(t, err, "treshold")

	for hcl, want := range map[string]string{
		`seal "awskms" {}`: "not implemented yet",
		`seal "shares" { threshold = 4
  shares = 3 }`: "threshold must be between",
		`seal "shares" {}
seal "aead" {}`: "second seal is not supported yet",
		`seal "rot13" {}`: "unknown seal type",
	} {
		bad, err := parseTestConfig(t, "server {\n keyring {\n"+hcl+"\n }\n}\n")
		must.NoError(t, err)
		merged = DevConfig(nil).Merge(bad)
		must.NoError(t, merged.normalizeAddrs())
		_, err = convertServerConfig(merged)
		must.ErrorContains(t, err, want)
	}
}
