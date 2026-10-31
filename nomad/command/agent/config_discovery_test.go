// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
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
}
`)
	must.NoError(t, err)
	must.True(t, c.Client.NativeServiceDiscovery.HealthyOnly())

	merged := DefaultConfig().Merge(c)
	must.True(t, merged.Client.NativeServiceDiscovery.HealthyOnly())

	_, err = parseTestConfig(t, `
client {
  native_service_discovery {
    default_healthy = true
  }
}
`)
	must.ErrorContains(t, err, "default_healthy")

	// the server's default overflow fraction
	c, err = parseTestConfig(t, `
server {
  native_service_discovery {
    default_overflow_fraction = 0.3
  }
}
`)
	must.NoError(t, err)
	must.Eq(t, pointer.Of(0.3), c.Server.NativeServiceDiscovery.DefaultOverflowFraction)

	// merging keeps the set value, a later file overrides it, an unset one
	// does not
	merged = DefaultConfig().Merge(c)
	must.Eq(t, pointer.Of(0.3), merged.Server.NativeServiceDiscovery.DefaultOverflowFraction)
	zero, err := parseTestConfig(t, `server { native_service_discovery { default_overflow_fraction = 0 } }`)
	must.NoError(t, err)
	must.Eq(t, pointer.Of(0.0), merged.Merge(zero).Server.NativeServiceDiscovery.DefaultOverflowFraction)
	empty, err := parseTestConfig(t, `server { native_service_discovery {} }`)
	must.NoError(t, err)
	must.Eq(t, pointer.Of(0.3), merged.Merge(empty).Server.NativeServiceDiscovery.DefaultOverflowFraction)

	// the server configuration carries it; without the block it is 0.2
	serverConf := func(c *Config) float64 {
		conf := DevConfig(nil)
		if c != nil {
			conf = conf.Merge(c)
		}
		must.NoError(t, conf.normalizeAddrs())
		sconf, err := convertServerConfig(conf)
		must.NoError(t, err)
		return sconf.DiscoveryDefaultOverflowFraction
	}
	must.Eq(t, 0.3, serverConf(c))
	must.Eq(t, 0.0, serverConf(zero))
	must.Eq(t, structs.DefaultDiscoveryOverflowFraction, serverConf(nil))

	// out of range values are rejected by the agent and config validate
	for _, bad := range []float64{-0.1, 1.01} {
		badConf := merged.Copy()
		badConf.Server.NativeServiceDiscovery.DefaultOverflowFraction = pointer.Of(bad)
		_, err = convertServerConfig(badConf)
		must.ErrorContains(t, err, "default_overflow_fraction must be between 0 and 1")
		badConf.Server.Enabled = true
		must.ErrorContains(t, validateNorcaBlocks(badConf), "default_overflow_fraction")
	}

	_, err = parseTestConfig(t, `
server {
  native_service_discovery {
    default_overflow = 0.3
  }
}
`)
	must.ErrorContains(t, err, "default_overflow")

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

	c, err = parseTestConfig(t, `
client {
  consul_compat {
    enabled         = true
    bind            = "127.0.0.1:8501"
    datacenter_from = "datacenter"
    namespaces      = ["default", "prod"]
    anonymous_token = "anon"
  }
}
`)
	must.NoError(t, err)
	compat := DefaultConfig().Merge(c).Client.ConsulCompat
	must.True(t, compat.IsEnabled())
	must.Eq(t, "127.0.0.1:8501", compat.BindAddr())
	must.False(t, compat.DatacenterIsRegion())
	must.Eq(t, []string{"default", "prod"}, compat.NamespaceList())
	clientConf, err = convertClientConfig(DefaultConfig().Merge(c))
	must.NoError(t, err)
	must.Eq(t, "anon", clientConf.ConsulCompat.AnonymousToken)

	bad = DefaultConfig().Merge(c)
	bad.Client.ConsulCompat.DatacenterFrom = "rack"
	_, err = convertClientConfig(bad)
	must.ErrorContains(t, err, "datacenter_from")

	// the kv block
	c, err = parseTestConfig(t, `
client {
  consul_compat {
    enabled = true
    kv {
      backend   = "nats"
      namespace = "prod"
      path      = "/shared/kv/"
      nats {
        urls          = ["nats://10.0.0.1:4222", "nats://10.0.0.2:4222"]
        bucket        = "kv"
        create_bucket = false
        replicas      = 3
        user          = "kv"
        password      = "pw"
        ca_file       = "/etc/nats/ca.pem"
      }
    }
  }
}
`)
	must.NoError(t, err)
	kv := DefaultConfig().Merge(c).Client.ConsulCompat.KV
	must.Eq(t, config.ConsulCompatKVNATS, kv.BackendName())
	must.Eq(t, "prod", kv.Namespace)
	must.Eq(t, "shared/kv", kv.PathPrefix())
	must.Eq(t, []string{"nats://10.0.0.1:4222", "nats://10.0.0.2:4222"}, kv.NATS.URLs)
	must.Eq(t, "kv", kv.NATS.BucketName())
	must.False(t, kv.NATS.CreatesBucket())
	must.Eq(t, 3, kv.NATS.Replicas)
	clientConf, err = convertClientConfig(DefaultConfig().Merge(c))
	must.NoError(t, err)
	must.Eq(t, "pw", clientConf.ConsulCompat.KV.NATS.Password)

	bad = DefaultConfig().Merge(c)
	bad.Client.ConsulCompat.KV.NATS.URLs = nil
	_, err = convertClientConfig(bad)
	must.ErrorContains(t, err, "needs nats.urls")
	bad.Client.ConsulCompat.KV.Backend = "etcd"
	_, err = convertClientConfig(bad)
	must.ErrorContains(t, err, "kv backend must be")

	var noKV *config.ConsulCompatKVConfig
	must.Eq(t, config.ConsulCompatKVAuto, noKV.BackendName())
	must.Eq(t, config.DefaultConsulCompatKVPath, noKV.PathPrefix())
	var noNATS *config.ConsulCompatKVNATSConfig
	must.Eq(t, config.DefaultConsulCompatKVBucket, noNATS.BucketName())
	must.True(t, noNATS.CreatesBucket())

	var noCompat *config.ConsulCompatConfig
	must.False(t, noCompat.IsEnabled())
	must.True(t, noCompat.DatacenterIsRegion())
	must.Eq(t, config.DefaultConsulCompatBind, noCompat.BindAddr())

	// defaults
	var none *config.DNSConfig
	must.False(t, none.IsEnabled())
	must.Eq(t, config.DefaultDNSBind, none.BindAddr())
	must.Eq(t, "nomad", none.DomainName())
	must.Eq(t, []string{"default"}, none.NamespaceList())
	must.Eq(t, config.DefaultDNSTTL, none.TTLDuration())
	must.False(t, DefaultConfig().Client.NativeServiceDiscovery.HealthyOnly())
}
