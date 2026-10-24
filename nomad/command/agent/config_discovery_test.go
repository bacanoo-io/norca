// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"

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

	// defaults
	must.False(t, DefaultConfig().Client.NativeServiceDiscovery.HealthyOnly())
	must.Eq(t, config.DefaultTopologyZone, DefaultConfig().Client.Topology.ZoneSource())
}
