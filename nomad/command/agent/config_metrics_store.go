// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/metricsstore"
)

// MetricsStoreConfig is the server's metrics_store block: the history of the
// task group usage the clients publish on the system bus, kept in
// <data_dir>/server/tsdb.
//
//	metrics_store {
//	  enabled            = true
//	  retention          = "720h"
//	  partition_duration = "6h"
//	  sample_interval    = "30s"
//	  max_disk_size      = "2GiB"
//	  max_series         = 50000
//	  wal_buffered_size  = 4096
//	}
type MetricsStoreConfig struct {
	// Enabled stores the usage; true by default.
	Enabled *bool `hcl:"enabled"`

	Retention         string `hcl:"retention"`
	PartitionDuration string `hcl:"partition_duration"`
	SampleInterval    string `hcl:"sample_interval"`
	MaxDiskSize       string `hcl:"max_disk_size"`
	MaxSeries         int    `hcl:"max_series"`
	WALBufferedSize   *int   `hcl:"wal_buffered_size"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

func (m *MetricsStoreConfig) Copy() *MetricsStoreConfig {
	if m == nil {
		return nil
	}
	nm := *m
	nm.Enabled = pointer.Copy(m.Enabled)
	nm.WALBufferedSize = pointer.Copy(m.WALBufferedSize)
	nm.ExtraKeysHCL = slices.Clone(m.ExtraKeysHCL)
	return &nm
}

// Merge merges b into a copy of a.
func (a *MetricsStoreConfig) Merge(b *MetricsStoreConfig) *MetricsStoreConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b == nil {
		return result
	}
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.Retention != "" {
		result.Retention = b.Retention
	}
	if b.PartitionDuration != "" {
		result.PartitionDuration = b.PartitionDuration
	}
	if b.SampleInterval != "" {
		result.SampleInterval = b.SampleInterval
	}
	if b.MaxDiskSize != "" {
		result.MaxDiskSize = b.MaxDiskSize
	}
	if b.MaxSeries != 0 {
		result.MaxSeries = b.MaxSeries
	}
	if b.WALBufferedSize != nil {
		result.WALBufferedSize = pointer.Copy(b.WALBufferedSize)
	}
	return result
}

// toServerConfig converts the block into the server's configuration,
// applying the defaults and validating it.
func (m *MetricsStoreConfig) toServerConfig() (*metricsstore.Config, error) {
	cfg := metricsstore.DefaultConfig()
	if m == nil {
		return cfg, nil
	}
	if m.Enabled != nil {
		cfg.Enabled = *m.Enabled
	}
	durations := []struct {
		name  string
		value string
		field *time.Duration
	}{
		{"retention", m.Retention, &cfg.Retention},
		{"partition_duration", m.PartitionDuration, &cfg.PartitionDuration},
		{"sample_interval", m.SampleInterval, &cfg.SampleInterval},
	}
	for _, d := range durations {
		if d.value == "" {
			continue
		}
		v, err := time.ParseDuration(d.value)
		if err != nil {
			return nil, fmt.Errorf("metrics_store.%s can't parse duration %s", d.name, d.value)
		}
		*d.field = v
	}
	if m.MaxDiskSize != "" {
		v, err := humanize.ParseBytes(m.MaxDiskSize)
		if err != nil || v > math.MaxInt64 {
			return nil, fmt.Errorf("metrics_store.max_disk_size can't parse size %s", m.MaxDiskSize)
		}
		cfg.MaxDiskSize = int64(v)
	}
	if m.MaxSeries != 0 {
		cfg.MaxSeries = m.MaxSeries
	}
	if m.WALBufferedSize != nil {
		cfg.WALBufferedSize = *m.WALBufferedSize
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metrics_store: %w", err)
	}
	return cfg, nil
}
