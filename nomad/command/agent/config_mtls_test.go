// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestConfig_ParseServerMTLS(t *testing.T) {
	ci.Parallel(t)

	c, err := parseTestConfig(t, `
server {
  mtls {
    trust_domain  = "prod.example"
    leaf_cert_ttl = "12h"
    ca_cert_ttl   = "87600h"
    federation    = false
  }
}
`)
	must.NoError(t, err)
	merged := DefaultConfig().Merge(c)
	conf := merged.Server.MTLS.Copy()
	must.NoError(t, conf.Finalize())
	must.Eq(t, "prod.example", conf.TrustDomain)
	must.Eq(t, 12*time.Hour, conf.LeafCertTTL)
	must.Eq(t, 87600*time.Hour, conf.CACertTTL)
	must.False(t, conf.FederationEnabled())
	must.True(t, conf.IsEnabled())

	merged = DevConfig(nil).Merge(c)
	must.NoError(t, merged.normalizeAddrs())
	serverConf, err := convertServerConfig(merged)
	must.NoError(t, err)
	must.Eq(t, 12*time.Hour, serverConf.MTLSConfig.LeafCertTTL)

	_, err = parseTestConfig(t, `
server {
  mtls {
    leaf_ttl = "12h"
  }
}
`)
	must.ErrorContains(t, err, "leaf_ttl")

	bad, err := parseTestConfig(t, `
server {
  mtls {
    trust_domain = "Not Valid"
  }
}
`)
	must.NoError(t, err)
	merged = DevConfig(nil).Merge(bad)
	must.NoError(t, merged.normalizeAddrs())
	_, err = convertServerConfig(merged)
	must.ErrorContains(t, err, "invalid trust domain")

	short, err := parseTestConfig(t, `
server {
  mtls {
    leaf_cert_ttl = "1m"
  }
}
`)
	must.NoError(t, err)
	merged = DevConfig(nil).Merge(short)
	must.NoError(t, merged.normalizeAddrs())
	_, err = convertServerConfig(merged)
	must.ErrorContains(t, err, "leaf_cert_ttl must be at least")
}
