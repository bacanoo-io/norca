// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

func parseTestConfig(t *testing.T, hcl string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
	return ParseConfigFile(path)
}

func TestConfig_ParseServiceDiscovery(t *testing.T) {
	ci.Parallel(t)

	c, err := parseTestConfig(t, `
client {
  native_service_discovery {
    default_healthy_only = true
  }
  topology {
    zone = "${attr.platform.aws.placement.availability-zone}"
  }
}
`)
	must.NoError(t, err)
	must.True(t, c.Client.NativeServiceDiscovery.HealthyOnly())
	must.Eq(t, "${attr.platform.aws.placement.availability-zone}", c.Client.Topology.ZoneSource())

	merged := DefaultConfig().Merge(c)
	must.True(t, merged.Client.NativeServiceDiscovery.HealthyOnly())
	must.Eq(t, "${attr.platform.aws.placement.availability-zone}", merged.Client.Topology.ZoneSource())

	_, err = parseTestConfig(t, `
client {
  native_service_discovery {
    default_healthy = true
  }
}
`)
	must.ErrorContains(t, err, "default_healthy")

	c, err = parseTestConfig(t, `
client {
  dns {
    enabled       = true
    bind          = "127.0.0.1:8653"
    domain        = "cluster."
    consul_compat = true
    recursors     = ["10.0.0.2", "10.0.0.3:5353"]
    ttl           = "10s"
    namespaces    = ["default", "prod"]
    token         = "secret"
  }
}
`)
	must.NoError(t, err)
	dns := DefaultConfig().Merge(c).Client.DNS
	must.True(t, dns.IsEnabled())
	must.True(t, dns.IsConsulCompat())
	must.Eq(t, "127.0.0.1:8653", dns.BindAddr())
	must.Eq(t, "cluster", dns.DomainName())
	must.Eq(t, []string{"10.0.0.2:53", "10.0.0.3:5353"}, dns.RecursorAddrs())
	must.Eq(t, 10*time.Second, dns.TTLDuration())
	must.Eq(t, []string{"default", "prod"}, dns.NamespaceList())
	must.NoError(t, dns.Validate())

	clientConf, err := convertClientConfig(DefaultConfig().Merge(c))
	must.NoError(t, err)
	must.Eq(t, "secret", clientConf.DNS.Token)

	bad := DefaultConfig().Merge(c)
	bad.Client.DNS.TTL = "soon"
	_, err = convertClientConfig(bad)
	must.ErrorContains(t, err, "dns ttl")

	// defaults
	var none *config.DNSConfig
	must.False(t, none.IsEnabled())
	must.Eq(t, config.DefaultDNSBind, none.BindAddr())
	must.Eq(t, "nomad", none.DomainName())
	must.Eq(t, []string{"default"}, none.NamespaceList())
	must.Eq(t, config.DefaultDNSTTL, none.TTLDuration())
	must.False(t, DefaultConfig().Client.NativeServiceDiscovery.HealthyOnly())
	must.Eq(t, config.DefaultTopologyZone, DefaultConfig().Client.Topology.ZoneSource())
}
