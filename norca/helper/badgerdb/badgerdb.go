// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package badgerdb opens the Badger databases of Norca (storage and RPC
// plan): the raft log of the servers (helper/raftbadger) and the state of
// the clients (client/state). It holds what both share: the options with
// their defaults, durable writes, and the background loops for the value
// log garbage collection and the metrics.
package badgerdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/hashicorp/go-hclog"
)

// Options configures a database. Zero values take the defaults given to
// Open (ServerDefaults or ClientDefaults).
type Options struct {
	Path   string
	Logger hclog.Logger

	// MetricsName names the database in the metrics (nomad.badger.<name>.*).
	MetricsName string

	MemTableSizeMB          int
	NumMemtables            int
	NumLevelZeroTables      int
	NumLevelZeroTablesStall int
	NumCompactors           int
	BaseTableSizeMB         int
	BaseLevelSizeMB         int
	BlockCacheSizeMB        int
	IndexCacheSizeMB        int
	ValueLogFileSizeMB      int
	ValueLogGCInterval      time.Duration

	// InMemory keeps everything in memory (tests and -dev agents).
	InMemory bool

	// ReadOnly opens an existing database without writing to it; it fails
	// while an agent holds the database.
	ReadOnly bool
}

// ServerDefaults are the starting values for the raft log of a server.
var ServerDefaults = Options{
	MetricsName:             "raft",
	MemTableSizeMB:          64,
	NumMemtables:            3,
	NumLevelZeroTables:      8,
	NumLevelZeroTablesStall: 24,
	NumCompactors:           4,
	BaseTableSizeMB:         4,
	BaseLevelSizeMB:         64,
	BlockCacheSizeMB:        128,
	IndexCacheSizeMB:        64,
	ValueLogFileSizeMB:      256,
	ValueLogGCInterval:      10 * time.Minute,
}

// ClientDefaults are the starting values for the state of a client: a few
// megabytes of allocation and task state, written in small transactions.
var ClientDefaults = Options{
	MetricsName:             "client",
	MemTableSizeMB:          16,
	NumMemtables:            2,
	NumLevelZeroTables:      5,
	NumLevelZeroTablesStall: 15,
	NumCompactors:           2,
	BaseTableSizeMB:         2,
	BaseLevelSizeMB:         10,
	BlockCacheSizeMB:        16,
	IndexCacheSizeMB:        8,
	ValueLogFileSizeMB:      64,
	ValueLogGCInterval:      10 * time.Minute,
}

// WithDefaults fills the zero values of o from d.
func (o Options) WithDefaults(d Options) Options {
	pick := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	pick(&o.MemTableSizeMB, d.MemTableSizeMB)
	pick(&o.NumMemtables, d.NumMemtables)
	pick(&o.NumLevelZeroTables, d.NumLevelZeroTables)
	pick(&o.NumLevelZeroTablesStall, d.NumLevelZeroTablesStall)
	pick(&o.NumCompactors, d.NumCompactors)
	pick(&o.BaseTableSizeMB, d.BaseTableSizeMB)
	pick(&o.BaseLevelSizeMB, d.BaseLevelSizeMB)
	pick(&o.BlockCacheSizeMB, d.BlockCacheSizeMB)
	pick(&o.IndexCacheSizeMB, d.IndexCacheSizeMB)
	pick(&o.ValueLogFileSizeMB, d.ValueLogFileSizeMB)
	if o.ValueLogGCInterval <= 0 {
		o.ValueLogGCInterval = d.ValueLogGCInterval
	}
	if o.MetricsName == "" {
		o.MetricsName = d.MetricsName
	}
	if o.NumLevelZeroTablesStall <= o.NumLevelZeroTables {
		o.NumLevelZeroTablesStall = o.NumLevelZeroTables + 1
	}
	return o
}

// valueThreshold keeps values up to 1 MB in the LSM tree; Badger allows at
// most 15% of a memtable, so a small memtable lowers it.
func (o Options) valueThreshold() int64 {
	limit := int64(o.MemTableSizeMB) << 20 * 15 / 100
	return min(int64(1<<20), limit)
}

func (o Options) badgerOptions(logger hclog.Logger) badger.Options {
	const mb = 1 << 20
	b := badger.DefaultOptions(o.Path).
		WithInMemory(o.InMemory).
		// Badger has no read-only mode on Windows; there the database is
		// opened normally, which takes the directory lock just the same,
		// and ReadOnly only keeps Norca from writing to it.
		WithReadOnly(o.ReadOnly && runtime.GOOS != "windows").
		WithSyncWrites(!o.InMemory).
		WithDetectConflicts(false).
		WithNumVersionsToKeep(1).
		WithMemTableSize(int64(o.MemTableSizeMB) * mb).
		WithNumMemtables(o.NumMemtables).
		WithNumLevelZeroTables(o.NumLevelZeroTables).
		WithNumLevelZeroTablesStall(o.NumLevelZeroTablesStall).
		WithNumCompactors(o.NumCompactors).
		WithBaseTableSize(int64(o.BaseTableSizeMB) * mb).
		WithBaseLevelSize(int64(o.BaseLevelSizeMB) * mb).
		WithBlockCacheSize(int64(o.BlockCacheSizeMB) * mb).
		WithIndexCacheSize(int64(o.IndexCacheSizeMB) * mb).
		WithValueLogFileSize(int64(o.ValueLogFileSizeMB) * mb).
		WithValueThreshold(o.valueThreshold()).
		WithCompression(options.Snappy).
		WithCompactL0OnClose(false).
		WithLogger(badgerLogger{logger.Named("badger")})
	if o.InMemory {
		b = b.WithDir("").WithValueDir("")
	}
	return b
}

// DB is an open Badger database with its background loops.
type DB struct {
	*badger.DB
	Opts   Options
	Logger hclog.Logger

	stop      chan struct{}
	closeOnce sync.Once
	done      sync.WaitGroup
}

// Open opens or creates the database in opts.Path; zero options take the
// values of defaults.
func Open(opts, defaults Options) (*DB, error) {
	opts = opts.WithDefaults(defaults)
	if opts.Path == "" && !opts.InMemory {
		return nil, errors.New("badgerdb: no path")
	}
	logger := opts.Logger
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	bdb, err := badger.Open(opts.badgerOptions(logger))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", opts.Path, err)
	}
	db := &DB{DB: bdb, Opts: opts, Logger: logger, stop: make(chan struct{})}
	if opts.ReadOnly {
		return db, nil
	}
	db.done.Add(2)
	go db.gcLoop()
	go db.metricsLoop()
	return db, nil
}

// Close stops the background loops and closes the database.
func (db *DB) Close() error {
	db.closeOnce.Do(func() { close(db.stop) })
	db.done.Wait()
	return db.DB.Close()
}

// IsDB reports whether dir holds a Badger database (its MANIFEST file).
func IsDB(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "MANIFEST"))
	return err == nil && st.Mode().IsRegular()
}

// CheckVersion compares the format version stored under key with version.
// A new database gets the version written; a read-only one without it is
// reported as not being a database of the expected kind.
func (db *DB) CheckVersion(key []byte, version uint64, kind string) error {
	check := db.Update
	if db.Opts.ReadOnly {
		check = db.View
	}
	return check(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if errors.Is(err, badger.ErrKeyNotFound) {
			if db.Opts.ReadOnly {
				return fmt.Errorf("%s is not a %s", db.Opts.Path, kind)
			}
			return txn.Set(key, Uint64Bytes(version))
		}
		if err != nil {
			return err
		}
		v, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		if len(v) != 8 || binary.BigEndian.Uint64(v) != version {
			return fmt.Errorf("%s: %s has an unknown format version", kind, db.Opts.Path)
		}
		return nil
	})
}

// Uint64Bytes encodes v big endian, so that keys sort by value.
func Uint64Bytes(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
