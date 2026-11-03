// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package badgerdb

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4/options"
)

// Kind is what a database holds; each kind has its own profiles.
type Kind string

const (
	// Server is the raft log of a server.
	Server Kind = "server"
	// Client is the state of a client.
	Client Kind = "client"
)

// Profiles are the defaults by the size of the machine: "small" fits agents
// with 2-4 GB of memory, "default" the usual servers and clients, "large"
// servers with plenty of memory and a busy raft log. Options set in the
// configuration override the profile's.
//
// The raft log keeps values above 4 KB in the value log: large entries in
// large batches would otherwise fill the memtables and be written again by
// every compaction; the log is deleted by ranges, so the value log garbage
// collection reclaims whole files. The client state rewrites the same keys
// all the time and keeps its values in the LSM tree (up to 1 MB).
var Profiles = map[string]map[Kind]Options{
	"small": {
		Server: {
			MetricsName:             "raft",
			ValueThresholdKB:        4,
			MemTableSizeMB:          16,
			NumMemtables:            2,
			NumLevelZeroTables:      5,
			NumLevelZeroTablesStall: 15,
			NumCompactors:           2,
			BaseTableSizeMB:         2,
			BaseLevelSizeMB:         16,
			BlockCacheSizeMB:        32,
			IndexCacheSizeMB:        16,
			ValueLogFileSizeMB:      64,
			ValueLogGCInterval:      10 * time.Minute,
			Compression:             "snappy",
		},
		Client: {
			MetricsName:             "client",
			MemTableSizeMB:          8,
			NumMemtables:            2,
			NumLevelZeroTables:      5,
			NumLevelZeroTablesStall: 15,
			NumCompactors:           2,
			BaseTableSizeMB:         2,
			BaseLevelSizeMB:         8,
			BlockCacheSizeMB:        8,
			IndexCacheSizeMB:        4,
			ValueLogFileSizeMB:      16,
			ValueLogGCInterval:      10 * time.Minute,
			Compression:             "snappy",
		},
	},
	"default": {
		Server: ServerDefaults,
		Client: ClientDefaults,
	},
	"large": {
		Server: {
			MetricsName:             "raft",
			ValueThresholdKB:        4,
			MemTableSizeMB:          128,
			NumMemtables:            4,
			NumLevelZeroTables:      10,
			NumLevelZeroTablesStall: 30,
			NumCompactors:           4,
			BaseTableSizeMB:         8,
			BaseLevelSizeMB:         128,
			BlockCacheSizeMB:        512,
			IndexCacheSizeMB:        256,
			ValueLogFileSizeMB:      512,
			ValueLogGCInterval:      10 * time.Minute,
			Compression:             "snappy",
		},
		Client: {
			MetricsName:             "client",
			MemTableSizeMB:          32,
			NumMemtables:            3,
			NumLevelZeroTables:      8,
			NumLevelZeroTablesStall: 24,
			NumCompactors:           2,
			BaseTableSizeMB:         4,
			BaseLevelSizeMB:         32,
			BlockCacheSizeMB:        64,
			IndexCacheSizeMB:        32,
			ValueLogFileSizeMB:      128,
			ValueLogGCInterval:      10 * time.Minute,
			Compression:             "snappy",
		},
	},
}

// Defaults returns the defaults of a profile for a kind ("" is "default").
func Defaults(kind Kind, profile string) (Options, error) {
	if profile == "" {
		profile = "default"
	}
	p, ok := Profiles[profile]
	if !ok {
		return Options{}, unknownProfile(profile)
	}
	d, ok := p[kind]
	if !ok {
		return Options{}, fmt.Errorf("badger profile %q has no %s values", profile, kind)
	}
	return d, nil
}

// ProfileNames lists the profiles.
func ProfileNames() []string {
	names := make([]string, 0, len(Profiles))
	for n := range Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func unknownProfile(name string) error {
	return fmt.Errorf("unknown badger profile %q (one of %s)", name, strings.Join(ProfileNames(), ", "))
}

// Validate checks the options a user may set.
func (o Options) Validate() error {
	if o.Profile != "" {
		if _, ok := Profiles[o.Profile]; !ok {
			return unknownProfile(o.Profile)
		}
	}
	switch o.Compression {
	case "", "snappy", "zstd", "none":
	default:
		return fmt.Errorf("unknown badger compression %q (snappy, zstd or none)", o.Compression)
	}
	if o.ValueLogFileSizeMB < 0 || o.ValueLogFileSizeMB >= 2048 {
		return fmt.Errorf("badger value_log_file_size_mb must be between 1 and 2047, not %d", o.ValueLogFileSizeMB)
	}
	if o.NumCompactors == 1 {
		return fmt.Errorf("badger num_compactors must be at least 2")
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"mem_table_size_mb", o.MemTableSizeMB},
		{"num_memtables", o.NumMemtables},
		{"num_level_zero_tables", o.NumLevelZeroTables},
		{"num_level_zero_tables_stall", o.NumLevelZeroTablesStall},
		{"num_compactors", o.NumCompactors},
		{"block_cache_size_mb", o.BlockCacheSizeMB},
		{"index_cache_size_mb", o.IndexCacheSizeMB},
		{"value_threshold_kb", o.ValueThresholdKB},
	} {
		if f.v < 0 {
			return fmt.Errorf("badger %s must not be negative, not %d", f.name, f.v)
		}
	}
	return nil
}

func compression(name string) options.CompressionType {
	switch name {
	case "zstd":
		return options.ZSTD
	case "none":
		return options.None
	default:
		return options.Snappy
	}
}
