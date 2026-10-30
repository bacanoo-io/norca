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
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/shoenig/test/must"
)

func TestConfig_Bus_Parse(t *testing.T) {
	ci.Parallel(t)

	hcl := `
ports {
  bus         = 5649
  bus_cluster = 5650
}
server {
  enabled = true
  bus {
    enabled         = true
    max_memory      = "128MiB"
    max_storage     = "2GiB"
    max_payload     = "2MiB"
    max_connections = 500
  }
}`
	path := filepath.Join(t.TempDir(), "bus.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
	parsed, err := ParseConfigFile(path)
	must.NoError(t, err)

	c := DefaultConfig().Merge(parsed)
	must.Eq(t, 5649, c.Ports.Bus)
	must.Eq(t, 5650, c.Ports.BusCluster)

	blocks, err := convertNorcaServerBlocks(c.Server, c.Ports)
	must.NoError(t, err)
	cfg := blocks.bus
	must.True(t, cfg.Enabled)
	must.Eq(t, 5649, cfg.Port)
	must.Eq(t, 5650, cfg.ClusterPort)
	must.Eq(t, int64(128<<20), cfg.MaxMemory)
	must.Eq(t, int64(2<<30), cfg.MaxStorage)
	must.Eq(t, int32(2<<20), cfg.MaxPayload)
	must.Eq(t, 500, cfg.MaxConnections)

	// Unknown keys are refused.
	must.NoError(t, os.WriteFile(path, []byte(`server { bus { max_msgs = 1 } }`), 0o600))
	_, err = ParseConfigFile(path)
	must.ErrorContains(t, err, "max_msgs")
}

func TestConfig_Bus_Defaults(t *testing.T) {
	ci.Parallel(t)

	// Without a bus block the bus is enabled on the default ports.
	c := DefaultConfig()
	blocks, err := convertNorcaServerBlocks(c.Server, c.Ports)
	must.NoError(t, err)
	must.True(t, blocks.bus.Enabled)
	must.Eq(t, bus.DefaultPort, blocks.bus.Port)
	must.Eq(t, bus.DefaultClusterPort, blocks.bus.ClusterPort)
	must.Eq(t, int64(bus.DefaultMaxStorage), blocks.bus.MaxStorage)

	// enabled = false turns it off; a later file merges field by field.
	off := (&BusConfig{Enabled: pointer.Of(false)}).Merge(&BusConfig{MaxMemory: "64MiB"})
	cfg, err := off.toServerConfig(c.Ports)
	must.NoError(t, err)
	must.False(t, cfg.Enabled)
	must.Eq(t, int64(64<<20), cfg.MaxMemory)
}

func TestConfig_Bus_Invalid(t *testing.T) {
	ci.Parallel(t)

	ports := DefaultConfig().Ports
	for name, b := range map[string]*BusConfig{
		"memory":      {MaxMemory: "lots"},
		"payload":     {MaxPayload: "128MiB"},
		"connections": {MaxConnections: -1},
	} {
		_, err := b.toServerConfig(ports)
		must.Error(t, err, must.Sprint(name))
	}

	same := ports.Copy()
	same.BusCluster = same.Bus
	_, err := (&BusConfig{}).toServerConfig(same)
	must.ErrorContains(t, err, "must differ")
}

func TestConfig_Bus_Events(t *testing.T) {
	ci.Parallel(t)

	hcl := `
server {
  enabled = true
  bus {
    max_storage = "8GiB"
    events {
      subject_prefix = "norca.events"
      max_age        = "24h"
      max_bytes      = "2GiB"
      topics         = ["Job", "Allocation"]
    }
  }
}`
	path := filepath.Join(t.TempDir(), "bus.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
	parsed, err := ParseConfigFile(path)
	must.NoError(t, err)
	c := DefaultConfig().Merge(parsed)
	blocks, err := convertNorcaServerBlocks(c.Server, c.Ports)
	must.NoError(t, err)
	ev := blocks.bus.Events
	must.True(t, ev.Enabled)
	must.Eq(t, "norca.events", ev.SubjectPrefix)
	must.Eq(t, 24*time.Hour, ev.MaxAge)
	must.Eq(t, int64(2<<30), ev.MaxBytes)
	must.Eq(t, []string{"Job", "Allocation"}, ev.Topics)

	// Without events.max_bytes the stream takes at most a quarter of the
	// storage.
	small, err := (&BusConfig{MaxStorage: "1GiB"}).toServerConfig(c.Ports)
	must.NoError(t, err)
	must.Eq(t, int64(256<<20), small.Events.MaxBytes)

	// An explicit limit beyond the storage, a bad prefix and unknown keys
	// are refused.
	_, err = (&BusConfig{MaxStorage: "1GiB", Events: &BusEventsConfig{MaxBytes: "2GiB"}}).toServerConfig(c.Ports)
	must.ErrorContains(t, err, "must fit into bus.max_storage")
	_, err = (&BusConfig{Events: &BusEventsConfig{SubjectPrefix: "a..b"}}).toServerConfig(c.Ports)
	must.ErrorContains(t, err, "subject_prefix")
	must.NoError(t, os.WriteFile(path, []byte(`server { bus { events { stream = "X" } } }`), 0o600))
	_, err = ParseConfigFile(path)
	must.ErrorContains(t, err, "stream")
}
