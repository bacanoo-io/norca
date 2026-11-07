// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package raftbadger is the raft log store and stable store of the Norca
// servers on Badger (storage update plan, phase 2). It replaces
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
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/badgerdb"
	"github.com/bacanoo-io/norca/norca/helper/diskusage"
	"github.com/dgraph-io/badger/v4"
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

// Options configures the store; zero values take ServerDefaults.
type Options = badgerdb.Options

// ServerDefaults are the starting values for the raft store of a server.
var ServerDefaults = badgerdb.ServerDefaults

// Store is a raft log store and stable store on Badger.
type Store struct {
	db *badgerdb.DB

	commitLock   sync.Mutex
	stagedCommit uint64 // written with the next StoreLogs
}

// Open opens or creates the store in opts.Path.
func Open(opts Options) (*Store, error) {
	db, err := badgerdb.Open(opts, badgerdb.Server)
	if err != nil {
		return nil, fmt.Errorf("raftbadger: %w", err)
	}
	if err := db.CheckVersion(keyVersion, formatVersion, "raft store"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("raftbadger: %w", err)
	}
	return &Store{db: db}, nil
}

// Close stops the background loops and closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// DiskSize returns the bytes the store takes on disk: every file of the
// database, that is the tables of the LSM tree, the value log and the logs of
// the memtables. It is measured at the call. Badger's own Size is not used:
// it is measured when the database opens, before the value log exists, and
// then once a minute; it takes the length of the value log, which Badger
// extends far ahead of the data; and it leaves out the memtables, which hold
// the newest entries. A store in memory has no size on disk: an error.
func (s *Store) DiskSize() (int64, error) {
	if s.db.Opts.InMemory {
		return 0, errors.New("raftbadger: the store is in memory")
	}
	return diskusage.Path(s.db.Opts.Path)
}

// IsStore reports whether dir holds a Badger database (its MANIFEST file).
func IsStore(dir string) bool {
	return badgerdb.IsDB(dir)
}

func logKey(idx uint64) []byte {
	k := make([]byte, len(prefixLog)+8)
	copy(k, prefixLog)
	binary.BigEndian.PutUint64(k[len(prefixLog):], idx)
	return k
}

func uint64Bytes(v uint64) []byte { return badgerdb.Uint64Bytes(v) }

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
	defer func() { s.db.MeasureSince("store_logs", start) }()

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
	defer func() { s.db.MeasureSince("delete_range", start) }()

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
