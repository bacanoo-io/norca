// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package raftbadger

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(Options{Path: dir, MemTableSizeMB: 4, BlockCacheSizeMB: 8, IndexCacheSizeMB: 4})
	must.NoError(t, err)
	return s
}

func entry(i uint64, data string) *raft.Log {
	return &raft.Log{Index: i, Term: 2, Type: raft.LogCommand, Data: []byte(data),
		AppendedAt: time.Date(2026, 11, 3, 1, 2, 3, 4, time.UTC)}
}

func TestStore_logs(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()

	first, err := s.FirstIndex()
	must.NoError(t, err)
	must.Eq(t, uint64(0), first)
	last, err := s.LastIndex()
	must.NoError(t, err)
	must.Eq(t, uint64(0), last)

	var logs []*raft.Log
	for i := uint64(1); i <= 300; i++ {
		logs = append(logs, entry(i, "data"))
	}
	must.NoError(t, s.StoreLogs(logs))
	must.NoError(t, s.StoreLog(entry(301, "one more")))

	first, _ = s.FirstIndex()
	last, _ = s.LastIndex()
	must.Eq(t, uint64(1), first)
	must.Eq(t, uint64(301), last)

	var got raft.Log
	must.NoError(t, s.GetLog(301, &got))
	must.Eq(t, "one more", string(got.Data))
	must.Eq(t, uint64(2), got.Term)
	must.True(t, got.AppendedAt.Equal(entry(1, "").AppendedAt))

	must.True(t, errors.Is(s.GetLog(999, &got), raft.ErrLogNotFound))

	// truncation after a snapshot
	must.NoError(t, s.DeleteRange(1, 250))
	first, _ = s.FirstIndex()
	must.Eq(t, uint64(251), first)
	must.True(t, errors.Is(s.GetLog(250, &got), raft.ErrLogNotFound))
	must.NoError(t, s.GetLog(251, &got))
}

func TestStore_largeBatchIsSplit(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()

	// more than one transaction holds with a 4 MB memtable
	data := string(bytes.Repeat([]byte("x"), 64<<10))
	var logs []*raft.Log
	for i := uint64(1); i <= 400; i++ {
		logs = append(logs, entry(i, data))
	}
	must.NoError(t, s.StoreLogs(logs))
	last, _ := s.LastIndex()
	must.Eq(t, uint64(400), last)
	must.NoError(t, s.DeleteRange(1, 400))
	first, _ := s.FirstIndex()
	must.Eq(t, uint64(0), first)
}

func TestStore_stable(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()

	_, err := s.Get([]byte("missing"))
	must.EqError(t, err, "not found") // raft compares the text
	_, err = s.GetUint64([]byte("missing"))
	must.EqError(t, err, "not found")

	must.NoError(t, s.Set([]byte("k"), []byte("v")))
	v, err := s.Get([]byte("k"))
	must.NoError(t, err)
	must.Eq(t, "v", string(v))

	must.NoError(t, s.SetUint64([]byte("CurrentTerm"), 42))
	n, err := s.GetUint64([]byte("CurrentTerm"))
	must.NoError(t, err)
	must.Eq(t, uint64(42), n)
}

func TestStore_commitIndex(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()

	c, err := s.GetCommitIndex()
	must.NoError(t, err)
	must.Eq(t, uint64(0), c)

	// staged values are written with the next StoreLogs only
	must.NoError(t, s.StageCommitIndex(5))
	c, _ = s.GetCommitIndex()
	must.Eq(t, uint64(0), c)
	must.NoError(t, s.StoreLogs([]*raft.Log{entry(1, "a"), entry(2, "b"), entry(3, "c")}))

	// never above the last index
	c, _ = s.GetCommitIndex()
	must.Eq(t, uint64(3), c)
	must.NoError(t, s.StageCommitIndex(2))
	must.NoError(t, s.StoreLog(entry(4, "d")))
	c, _ = s.GetCommitIndex()
	must.Eq(t, uint64(2), c)
}

func TestStore_reopen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	must.NoError(t, s.StoreLogs([]*raft.Log{entry(1, "a"), entry(2, "b")}))
	must.NoError(t, s.SetUint64([]byte("CurrentTerm"), 7))
	must.NoError(t, s.Close())

	s = open(t, dir)
	defer s.Close()
	last, _ := s.LastIndex()
	must.Eq(t, uint64(2), last)
	n, err := s.GetUint64([]byte("CurrentTerm"))
	must.NoError(t, err)
	must.Eq(t, uint64(7), n)
}

func TestStore_inMemory(t *testing.T) {
	s, err := Open(Options{Path: "memory", InMemory: true})
	must.NoError(t, err)
	defer s.Close()
	must.NoError(t, s.StoreLog(entry(1, "a")))
	var got raft.Log
	must.NoError(t, s.GetLog(1, &got))
}

func TestOptions_defaults(t *testing.T) {
	o := Options{Path: "x"}.WithDefaults(ServerDefaults)
	must.Eq(t, ServerDefaults.NumLevelZeroTablesStall, o.NumLevelZeroTablesStall)
	must.Eq(t, "raft", o.MetricsName)

	// the stall threshold is always above the compaction threshold
	o = Options{Path: "x", NumLevelZeroTables: 30, NumLevelZeroTablesStall: 10}.WithDefaults(ServerDefaults)
	must.Eq(t, 31, o.NumLevelZeroTablesStall)
}
