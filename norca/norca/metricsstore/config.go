// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package metricsstore keeps the history of the task group usage the clients
// publish on the system bus: every server stores every report in an embedded
// time-series store (tstorage) in its data directory, apart from Raft, the
// state store and the backups. The reports are coalesced into one point per
// sample interval and task group before they are written.
package metricsstore

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

const (
	// DefaultRetention is how long the points are kept.
	DefaultRetention = 30 * 24 * time.Hour

	// MinRetention and MaxRetention bound the retention.
	MinRetention = time.Hour
	MaxRetention = 30 * 24 * time.Hour

	// DefaultPartitionDuration is the time range of a partition: the
	// points of the newest two partitions are in memory (and in the WAL).
	// A shorter retention shortens the default partition to the retention.
	DefaultPartitionDuration = 6 * time.Hour

	// DefaultSampleInterval is the resolution of the stored points.
	DefaultSampleInterval = 30 * time.Second

	// DefaultMaxDiskSize bounds the disk use; the oldest partitions are
	// removed first.
	DefaultMaxDiskSize = 5 << 30

	// DefaultMaxSeries bounds the number of series.
	DefaultMaxSeries = 50000

	// DefaultWALBufferedSize is the WAL buffer in bytes.
	DefaultWALBufferedSize = 4096
)

// Config configures the metrics store of a server.
type Config struct {
	// Enabled stores the usage reports. Disabled, nothing is stored and no
	// directory is created.
	Enabled bool

	// Dir is the directory of the store; empty keeps the points in memory
	// only (dev mode without a data directory).
	Dir string

	Retention         time.Duration
	PartitionDuration time.Duration
	SampleInterval    time.Duration

	// MaxDiskSize is the disk cap in bytes.
	MaxDiskSize int64

	// MaxSeries is the cap of the number of series.
	MaxSeries int

	// WALBufferedSize is the WAL buffer in bytes: 0 writes every batch,
	// -1 disables the WAL.
	WALBufferedSize int
}

// DefaultConfig returns the defaults: enabled.
func DefaultConfig() *Config {
	return &Config{
		Enabled:           true,
		Retention:         DefaultRetention,
		PartitionDuration: DefaultPartitionDuration,
		SampleInterval:    DefaultSampleInterval,
		MaxDiskSize:       DefaultMaxDiskSize,
		MaxSeries:         DefaultMaxSeries,
		WALBufferedSize:   DefaultWALBufferedSize,
	}
}

// Copy returns a copy of the configuration.
func (c *Config) Copy() *Config {
	if c == nil {
		return nil
	}
	nc := *c
	return &nc
}

// Merge returns c with the set fields of b.
func (c *Config) Merge(b *Config) *Config {
	if c == nil {
		return b.Copy()
	}
	out := c.Copy()
	if b == nil {
		return out
	}
	out.Enabled = b.Enabled
	if b.Dir != "" {
		out.Dir = b.Dir
	}
	if b.Retention != 0 {
		out.Retention = b.Retention
	}
	if b.PartitionDuration != 0 {
		out.PartitionDuration = b.PartitionDuration
	}
	if b.SampleInterval != 0 {
		out.SampleInterval = b.SampleInterval
	}
	if b.MaxDiskSize != 0 {
		out.MaxDiskSize = b.MaxDiskSize
	}
	if b.MaxSeries != 0 {
		out.MaxSeries = b.MaxSeries
	}
	if b.WALBufferedSize != 0 {
		out.WALBufferedSize = b.WALBufferedSize
	}
	return out
}

// Validate checks the limits.
func (c *Config) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	var errs []error
	if c.Retention < MinRetention || c.Retention > MaxRetention {
		errs = append(errs, fmt.Errorf("retention must be between 1h and 720h (30d), not %s", FormatRetention(c.Retention)))
	}
	if c.SampleInterval < time.Second {
		errs = append(errs, errors.New("sample_interval must be at least 1s"))
	}
	if c.PartitionDuration < c.SampleInterval {
		errs = append(errs, errors.New("partition_duration must not be shorter than sample_interval"))
	}
	if c.PartitionDuration > c.Retention {
		errs = append(errs, errors.New("partition_duration must not be longer than retention"))
	}
	if c.MaxDiskSize < 0 {
		errs = append(errs, errors.New("max_disk_size must not be negative"))
	}
	if c.MaxSeries < 0 {
		errs = append(errs, errors.New("max_series must not be negative"))
	}
	if c.WALBufferedSize < -1 {
		errs = append(errs, errors.New("wal_buffered_size must be -1 or more"))
	}
	return errors.Join(errs...)
}

// retentionDays matches a retention that starts with whole days: "7d",
// "1d12h".
var retentionDays = regexp.MustCompile(`^([0-9]+)d(.*)$`)

// ParseRetention parses a retention: a Go duration ("168h", "90m") or whole
// days, optionally followed by a Go duration ("7d", "1d12h").
func ParseRetention(s string) (time.Duration, error) {
	m := retentionDays.FindStringSubmatch(s)
	if m == nil {
		return time.ParseDuration(s)
	}
	days, err := strconv.ParseInt(m[1], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	d := time.Duration(days) * 24 * time.Hour
	if m[2] != "" {
		rest, err := time.ParseDuration(m[2])
		if err != nil || rest < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d += rest
	}
	return d, nil
}

// FormatRetention formats a retention of whole days as days ("7d"), any
// other as a Go duration.
func FormatRetention(d time.Duration) string {
	const day = 24 * time.Hour
	if d > 0 && d%day == 0 {
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}
	return d.String()
}
