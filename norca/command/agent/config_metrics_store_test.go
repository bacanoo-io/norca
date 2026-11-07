// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/shoenig/test/must"
)

func TestConfig_MetricsStore_Parse(t *testing.T) {
	ci.Parallel(t)

	hcl := `
server {
  enabled = true
  metrics_store {
    retention          = "7d"
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

// TestConfig_MetricsStore_Retention checks the range of the retention, its
// days, and the partition that follows a short retention.
func TestConfig_MetricsStore_Retention(t *testing.T) {
	ci.Parallel(t)

	for in, want := range map[string]struct{ retention, partition time.Duration }{
		"1h":    {time.Hour, time.Hour},
		"4h":    {4 * time.Hour, 4 * time.Hour},
		"7d":    {7 * 24 * time.Hour, 6 * time.Hour},
		"168h":  {168 * time.Hour, 6 * time.Hour},
		"720h":  {720 * time.Hour, 6 * time.Hour},
		"30d":   {720 * time.Hour, 6 * time.Hour},
		"1d12h": {36 * time.Hour, 6 * time.Hour},
	} {
		cfg, err := (&MetricsStoreConfig{Retention: in}).toServerConfig()
		must.NoError(t, err, must.Sprint(in))
		must.Eq(t, want.retention, cfg.Retention, must.Sprint(in))
		must.Eq(t, want.partition, cfg.PartitionDuration, must.Sprint(in))
	}

	// A partition_duration that is set is kept.
	cfg, err := (&MetricsStoreConfig{Retention: "2h", PartitionDuration: "30m"}).toServerConfig()
	must.NoError(t, err)
	must.Eq(t, 30*time.Minute, cfg.PartitionDuration)

	// Out of range: below an hour, above 30 days.
	for _, in := range []string{"30m", "31d", "721h"} {
		_, err := (&MetricsStoreConfig{Retention: in}).toServerConfig()
		must.ErrorContains(t, err, "retention must be between 1h and 720h (30d)", must.Sprint(in))
	}
	_, err = (&MetricsStoreConfig{Retention: "2h", PartitionDuration: "6h"}).toServerConfig()
	must.ErrorContains(t, err, "partition_duration must not be longer than retention")
}

func TestConfig_MetricsStore_Invalid(t *testing.T) {
	ci.Parallel(t)

	for name, m := range map[string]*MetricsStoreConfig{
		"retention":  {Retention: "1000h"},
		"31 days":    {Retention: "31d"},
		"30 minutes": {Retention: "30m"},
		"days":       {Retention: "7 days"},
		"partition":  {Retention: "2h", PartitionDuration: "6h"},
		"duration":   {SampleInterval: "soon"},
		"disk":       {MaxDiskSize: "lots"},
		"partitions": {PartitionDuration: "10s", SampleInterval: "30s"},
		"wal":        {WALBufferedSize: pointer.Of(-2)},
	} {
		_, err := m.toServerConfig()
		must.Error(t, err, must.Sprint(name))
	}
}
