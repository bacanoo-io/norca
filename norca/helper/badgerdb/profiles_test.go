// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package badgerdb

import (
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4/options"
	"github.com/shoenig/test/must"
)

func TestProfiles_complete(t *testing.T) {
	for _, name := range ProfileNames() {
		for _, kind := range []Kind{Server, Client} {
			d, err := Defaults(kind, name)
			must.NoError(t, err)
			must.NoError(t, d.Validate(), must.Sprintf("%s/%s", name, kind))
			must.Positive(t, d.MemTableSizeMB)
			must.Positive(t, d.NumMemtables)
			must.Positive(t, d.ValueLogFileSizeMB)
			must.Greater(t, d.NumLevelZeroTables, d.NumLevelZeroTablesStall)
			must.GreaterEq(t, 2, d.NumCompactors)
			must.NotEq(t, "", d.Compression)
			must.Eq(t, map[Kind]string{Server: "raft", Client: "client"}[kind], d.MetricsName)
		}
	}
	d, err := Defaults(Server, "")
	must.NoError(t, err)
	must.Eq(t, ServerDefaults, d)
}

func TestProfiles_overrideAndValidate(t *testing.T) {
	small, err := Defaults(Server, "small")
	must.NoError(t, err)
	o := Options{Profile: "small", BlockCacheSizeMB: 64}.WithDefaults(small)
	must.Eq(t, 64, o.BlockCacheSizeMB)
	must.Eq(t, small.MemTableSizeMB, o.MemTableSizeMB)

	_, err = Defaults(Server, "huge")
	must.ErrorContains(t, err, "unknown badger profile")
	must.ErrorContains(t, Options{Compression: "lz4"}.Validate(), "compression")
	must.ErrorContains(t, Options{NumCompactors: 1}.Validate(), "num_compactors")
	must.ErrorContains(t, Options{ValueLogFileSizeMB: 4096}.Validate(), "value_log_file_size_mb")
	must.ErrorContains(t, Options{ValueThresholdKB: -1}.Validate(), "value_threshold_kb")
	must.NoError(t, Options{Profile: "large", Compression: "zstd"}.Validate())
}

func TestOpen_profileAndOptions(t *testing.T) {
	db, err := Open(Options{
		Path:             filepath.Join(t.TempDir(), "db"),
		Profile:          "small",
		Compression:      "zstd",
		ValueThresholdKB: 4,
	}, Client)
	must.NoError(t, err)
	defer db.Close()

	must.Eq(t, 8, db.Opts.MemTableSizeMB)
	must.Eq(t, "client", db.Opts.MetricsName)
	bo := db.DB.Opts()
	must.Eq(t, options.ZSTD, bo.Compression)
	must.Eq(t, int64(4<<10), bo.ValueThreshold)
	must.Eq(t, int64(16<<20), bo.ValueLogFileSize)

	_, err = Open(Options{Path: t.TempDir(), Profile: "nope"}, Server)
	must.ErrorContains(t, err, "unknown badger profile")
}

func TestValueThreshold_cappedByMemtable(t *testing.T) {
	// 15% of a 4 MB memtable is below the 1 MB default
	o := Options{MemTableSizeMB: 4}
	must.Eq(t, int64(4<<20*15/100), o.valueThreshold())
	o.ValueThresholdKB = 4
	must.Eq(t, int64(4<<10), o.valueThreshold())
}

// TestProfiles_valueThreshold asserts the raft log moves values above 4 KB
// to the value log and the client state keeps them in the tree.
func TestProfiles_valueThreshold(t *testing.T) {
	for _, name := range ProfileNames() {
		s, err := Defaults(Server, name)
		must.NoError(t, err)
		must.Eq(t, 4, s.ValueThresholdKB)
		c, err := Defaults(Client, name)
		must.NoError(t, err)
		must.Eq(t, 0, c.ValueThresholdKB)
		must.Eq(t, int64(1<<20), c.WithDefaults(c).valueThreshold())
	}
}
