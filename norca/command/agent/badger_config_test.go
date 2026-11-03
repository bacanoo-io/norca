// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// TestBadgerConfig_parseAndConvert asserts the badger blocks of the server
// and the client reach the options of their databases.
func TestBadgerConfig_parseAndConvert(t *testing.T) {
	ci.Parallel(t)

	path := filepath.Join(t.TempDir(), "agent.hcl")
	must.NoError(t, os.WriteFile(path, []byte(`
server {
  enabled = true
  badger {
    mem_table_size_mb = 32
    value_log_gc_interval = "5m"
  }
}
client {
  enabled = true
  badger {
    mem_table_size_mb = 8
    num_level_zero_tables = 4
    num_level_zero_tables_stall = 12
    num_compactors = 2
    block_cache_size_mb = 4
    index_cache_size_mb = 2
    value_log_gc_interval = "30m"
  }
}
`), 0o600))
	parsed, err := ParseConfigFile(path)
	must.NoError(t, err)
	conf := DevConfig(nil).Merge(parsed)
	must.NoError(t, conf.normalizeAddrs())
	must.Eq(t, 8, conf.Client.Badger.MemTableSizeMB)

	sc, err := convertServerConfig(conf)
	must.NoError(t, err)
	must.Eq(t, 32, sc.RaftBadger.MemTableSizeMB)
	must.Eq(t, 5*time.Minute, sc.RaftBadger.ValueLogGCInterval)

	cc, err := convertClientConfig(conf)
	must.NoError(t, err)
	o := cc.StateBadger
	must.Eq(t, 8, o.MemTableSizeMB)
	must.Eq(t, 4, o.NumLevelZeroTables)
	must.Eq(t, 12, o.NumLevelZeroTablesStall)
	must.Eq(t, 2, o.NumCompactors)
	must.Eq(t, 4, o.BlockCacheSizeMB)
	must.Eq(t, 2, o.IndexCacheSizeMB)
	must.Eq(t, 30*time.Minute, o.ValueLogGCInterval)

	// Copy keeps the block apart from the original
	cp := conf.Client.Copy()
	cp.Badger.MemTableSizeMB = 99
	must.Eq(t, 8, conf.Client.Badger.MemTableSizeMB)

	conf.Client.Badger.ValueLogGCInterval = "soon"
	_, err = convertClientConfig(conf)
	must.ErrorContains(t, err, "invalid client badger value_log_gc_interval")
}
