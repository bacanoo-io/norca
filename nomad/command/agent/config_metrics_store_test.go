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
	"github.com/hashicorp/nomad/nomad/metricsstore"
	"github.com/shoenig/test/must"
)

func TestConfig_MetricsStore_Parse(t *testing.T) {
	ci.Parallel(t)

	hcl := `
server {
  enabled = true
  metrics_store {
    retention          = "168h"
    partition_duration = "2h"
    sample_interval    = "60s"
    max_disk_size      = "512MiB"
    max_series         = 1000
    wal_buffered_size  = 0
  }
}`
	path := filepath.Join(t.TempDir(), "metrics.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
	parsed, err := ParseConfigFile(path)
	must.NoError(t, err)

	c := DefaultConfig().Merge(parsed)
	blocks, err := convertNorcaServerBlocks(c.Server, c.Ports)
	must.NoError(t, err)
	cfg := blocks.metricsStore
	must.True(t, cfg.Enabled)
	must.Eq(t, 168*time.Hour, cfg.Retention)
	must.Eq(t, 2*time.Hour, cfg.PartitionDuration)
	must.Eq(t, time.Minute, cfg.SampleInterval)
	must.Eq(t, int64(512<<20), cfg.MaxDiskSize)
	must.Eq(t, 1000, cfg.MaxSeries)
	must.Eq(t, 0, cfg.WALBufferedSize)

	// Unknown keys are refused.
	must.NoError(t, os.WriteFile(path, []byte(`server { metrics_store { max_points = 1 } }`), 0o600))
	_, err = ParseConfigFile(path)
	must.ErrorContains(t, err, "max_points")
}

func TestConfig_MetricsStore_Defaults(t *testing.T) {
	ci.Parallel(t)

	// Without a block the store is enabled with the defaults.
	blocks, err := convertNorcaServerBlocks(DefaultConfig().Server, nil)
	must.NoError(t, err)
	must.Eq(t, metricsstore.DefaultConfig(), blocks.metricsStore)

	// enabled = false turns it off; a later file merges field by field.
	off := (&MetricsStoreConfig{Enabled: pointer.Of(false)}).Merge(&MetricsStoreConfig{Retention: "24h"})
	cfg, err := off.toServerConfig()
	must.NoError(t, err)
	must.False(t, cfg.Enabled)
	must.Eq(t, 24*time.Hour, cfg.Retention)
}

func TestConfig_MetricsStore_Invalid(t *testing.T) {
	ci.Parallel(t)

	for name, m := range map[string]*MetricsStoreConfig{
		"retention":  {Retention: "1000h"},
		"duration":   {SampleInterval: "soon"},
		"disk":       {MaxDiskSize: "lots"},
		"partitions": {PartitionDuration: "10s", SampleInterval: "30s"},
		"wal":        {WALBufferedSize: pointer.Of(-2)},
	} {
		_, err := m.toServerConfig()
		must.Error(t, err, must.Sprint(name))
	}
}
