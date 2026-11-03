// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package raftbadger is the raft log store and stable store of the Norca
// servers on Badger (storage and RPC plan, phase 2). It replaces
// raft-boltdb.
//
// Layout (all keys in one Badger database):
//
//	l/<index, 8 bytes big endian>   a raft log entry, MessagePack encoded
//	s/<key>                         a stable store value
//	c/commit                        the commit index (CommitTrackingLogStore)
//	m/version                       the format version of the store
//
// Writes are durable (SyncWrites): StoreLogs writes its entries in one
// transaction, so a batch costs one sync. The value log garbage collection
// runs in a background loop, never in the write path.
package raftbadger

import (
	"bytes"
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
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/raft"
)

const formatVersion = 1

var (
	prefixLog    = []byte("l/")
	prefixStable = []byte("s/")
	keyCommit    = []byte("c/commit")
	keyVersion   = []byte("m/version")

	// errNotFound is what raft expects from a stable store for a missing key.
	errNotFound = errors.New("not found")
)

var (
	_ raft.LogStore               = (*Store)(nil)
	_ raft.StableStore            = (*Store)(nil)
	_ raft.CommitTrackingLogStore = (*Store)(nil)
)

// Store is a raft log store and stable store on Badger.
type Store struct {
	db     *badger.DB
	opts   Options
	logger hclog.Logger

	commitLock   sync.Mutex
	stagedCommit uint64 // written with the next StoreLogs

	stop      chan struct{}
	closeOnce sync.Once
	done      sync.WaitGroup
}

// Open opens or creates the store in opts.Path.
func Open(opts Options) (*Store, error) {
	opts = opts.withDefaults()
	if opts.Path == "" {
		return nil, errors.New("raftbadger: no path")
	}
	logger := opts.Logger
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	db, err := badger.Open(opts.badgerOptions(logger))
	if err != nil {
		return nil, fmt.Errorf("raftbadger: opening %s: %w", opts.Path, err)
	}
	s := &Store{db: db, opts: opts, logger: logger, stop: make(chan struct{})}
	if err := s.checkVersion(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if opts.ReadOnly {
		return s, nil
	}
	s.done.Add(2)
	go s.gcLoop()
	go s.metricsLoop()
	return s, nil
}

// Close stops the background loops and closes the database.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	s.done.Wait()
	return s.db.Close()
}

// IsStore reports whether dir holds a Badger database (its MANIFEST file).
func IsStore(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "MANIFEST"))
	return err == nil && st.Mode().IsRegular()
}

func (s *Store) checkVersion() error {
	check := s.db.Update
	if s.opts.ReadOnly {
		check = s.db.View
	}
	return check(func(txn *badger.Txn) error {
		item, err := txn.Get(keyVersion)
		if errors.Is(err, badger.ErrKeyNotFound) {
			if s.opts.ReadOnly {
				return fmt.Errorf("raftbadger: %s is not a raft store", s.opts.Path)
			}
			return txn.Set(keyVersion, uint64Bytes(formatVersion))
		}
		if err != nil {
			return err
		}
		v, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		if len(v) != 8 || binary.BigEndian.Uint64(v) != formatVersion {
			return fmt.Errorf("raftbadger: %s has an unknown format version", s.opts.Path)
		}
		return nil
	})
}

func logKey(idx uint64) []byte {
	k := make([]byte, len(prefixLog)+8)
	copy(k, prefixLog)
	binary.BigEndian.PutUint64(k[len(prefixLog):], idx)
	return k
}

func uint64Bytes(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// --- LogStore -----------------------------------------------------------------

// FirstIndex returns the first index written, 0 for no entries.
func (s *Store) FirstIndex() (uint64, error) {
	return s.edgeIndex(false)
}

// LastIndex returns the last index written, 0 for no entries.
func (s *Store) LastIndex() (uint64, error) {
	return s.edgeIndex(true)
}

func (s *Store) edgeIndex(last bool) (uint64, error) {
	var idx uint64
	err := s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{PrefetchValues: false, Reverse: last, Prefix: prefixLog})
		defer it.Close()
		seek := prefixLog
		if last {
			seek = append(append([]byte{}, prefixLog...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		}
		it.Seek(seek)
		if it.ValidForPrefix(prefixLog) {
			idx = binary.BigEndian.Uint64(it.Item().Key()[len(prefixLog):])
		}
		return nil
	})
	return idx, err
}

// GetLog gets the log entry at index.
func (s *Store) GetLog(index uint64, log *raft.Log) error {
	return s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(logKey(index))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return raft.ErrLogNotFound
		}
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error { return decode(v, log) })
	})
}

// StoreLog stores one log entry.
func (s *Store) StoreLog(log *raft.Log) error {
	return s.StoreLogs([]*raft.Log{log})
}

// StoreLogs stores the entries and the staged commit index in one
// transaction (one sync); a batch too big for one transaction is split.
func (s *Store) StoreLogs(logs []*raft.Log) error {
	start := time.Now()
	defer func() { s.measureSince("store_logs", start) }()

	s.commitLock.Lock()
	commit := s.stagedCommit
	s.commitLock.Unlock()

	txn := s.db.NewTransaction(true)
	defer func() { txn.Discard() }()
	set := func(k, v []byte) error {
		err := txn.Set(k, v)
		if errors.Is(err, badger.ErrTxnTooBig) {
			if err := txn.Commit(); err != nil {
				return err
			}
			txn = s.db.NewTransaction(true)
			err = txn.Set(k, v)
		}
		return err
	}
	for _, l := range logs {
		v, err := encode(l)
		if err != nil {
			return err
		}
		if err := set(logKey(l.Index), v); err != nil {
			return err
		}
	}
	if commit > 0 {
		if err := set(keyCommit, uint64Bytes(commit)); err != nil {
			return err
		}
	}
	return txn.Commit()
}

// DeleteRange deletes the entries from min to max, both included, in
// transactions of the size Badger allows (no DropPrefix: it blocks writes).
func (s *Store) DeleteRange(min, max uint64) error {
	start := time.Now()
	defer func() { s.measureSince("delete_range", start) }()

	txn := s.db.NewTransaction(true)
	defer func() { txn.Discard() }()
	for i := min; i <= max; i++ {
		err := txn.Delete(logKey(i))
		if errors.Is(err, badger.ErrTxnTooBig) {
			if err := txn.Commit(); err != nil {
				return err
			}
			txn = s.db.NewTransaction(true)
			err = txn.Delete(logKey(i))
		}
		if err != nil {
			return err
		}
		if i == max { // max may be the largest uint64
			break
		}
	}
	return txn.Commit()
}

// --- CommitTrackingLogStore ------------------------------------------------------

// StageCommitIndex keeps idx to be written with the next StoreLogs.
func (s *Store) StageCommitIndex(idx uint64) error {
	s.commitLock.Lock()
	defer s.commitLock.Unlock()
	s.stagedCommit = idx
	return nil
}

// GetCommitIndex returns the stored commit index, never above the last index.
func (s *Store) GetCommitIndex() (uint64, error) {
	var commit uint64
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyCommit)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return item.Value(func(v []byte) error {
			if len(v) == 8 {
				commit = binary.BigEndian.Uint64(v)
			}
			return nil
		})
	})
	if err != nil || commit == 0 {
		return 0, err
	}
	last, err := s.LastIndex()
	if err != nil {
		return 0, err
	}
	return min(commit, last), nil
}

// --- StableStore --------------------------------------------------------------------

// Set stores a value of the stable store.
func (s *Store) Set(key, val []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(append(append([]byte{}, prefixStable...), key...), val)
	})
}

// Get returns a value of the stable store; a missing key is the error "not
// found", as raft expects.
func (s *Store) Get(key []byte) ([]byte, error) {
	var out []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(append(append([]byte{}, prefixStable...), key...))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		out, err = item.ValueCopy(nil)
		return err
	})
	return out, err
}

// SetUint64 stores a number of the stable store.
func (s *Store) SetUint64(key []byte, val uint64) error {
	return s.Set(key, uint64Bytes(val))
}

// GetUint64 returns a number of the stable store.
func (s *Store) GetUint64(key []byte) (uint64, error) {
	v, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("raftbadger: value of %q is not a uint64", key)
	}
	return binary.BigEndian.Uint64(v), nil
}

// --- encoding --------------------------------------------------------------------------

// The entries are encoded with the codec Norca uses everywhere (go-msgpack
// v1.1.5), as raft-boltdb did.
var handle = &codec.MsgpackHandle{}

func encode(l *raft.Log) ([]byte, error) {
	var buf bytes.Buffer
	err := codec.NewEncoder(&buf, handle).Encode(l)
	return buf.Bytes(), err
}

func decode(v []byte, l *raft.Log) error {
	return codec.NewDecoderBytes(v, handle).Decode(l)
}

// --- options ---------------------------------------------------------------------------

// Options configures the store. Zero values take the defaults below, the
// starting values of the storage and RPC plan for servers.
type Options struct {
	Path   string
	Logger hclog.Logger

	// MetricsName names the store in the metrics (nomad.badger.<name>.*).
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

	// ReadOnly opens an existing store without writing to it (the operator
	// raft commands); it fails while a server holds the store.
	ReadOnly bool
}

// ServerDefaults are the starting values for the raft store of a server.
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

func (o Options) withDefaults() Options {
	d := ServerDefaults
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
		// Badger has no read-only mode on Windows; there the store is opened
		// normally, which takes the directory lock just the same, and
		// ReadOnly only keeps Norca from writing to it.
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
