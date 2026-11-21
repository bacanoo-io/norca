// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package raftbadger

import (
	"errors"
	"math"
	"testing"

	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
)

// errStop stands for a crash between two transactions of a DeleteRange.
var errStop = errors.New("stopped")

// fill stores the entries 1..n.
func fill(t *testing.T, s *Store, n uint64) {
	t.Helper()
	var logs []*raft.Log
	for i := uint64(1); i <= n; i++ {
		logs = append(logs, entry(i, "x"))
		if len(logs) == 1000 {
			must.NoError(t, s.StoreLogs(logs))
			logs = nil
		}
	}
	must.NoError(t, s.StoreLogs(logs))
}

// contiguous checks that every entry from the first to the last index is there.
func contiguous(t *testing.T, s *Store) (first, last uint64) {
	t.Helper()
	first, err := s.FirstIndex()
	must.NoError(t, err)
	last, err = s.LastIndex()
	must.NoError(t, err)
	var got raft.Log
	for i := first; i != 0 && i <= last; i++ {
		must.NoError(t, s.GetLog(i, &got), must.Sprintf("hole at %d (log %d..%d)", i, first, last))
	}
	return first, last
}

// A truncation of the tail stopped between two of its transactions leaves the
// log without a hole (review finding HLP-8); so does a compaction of the head.
func TestStore_DeleteRangeStoppedLeavesNoHole(t *testing.T) {
	const n = 60000 // more deletions than one transaction of a 4 MB memtable holds

	t.Run("tail", func(t *testing.T) {
		s := open(t, t.TempDir())
		defer s.Close()
		fill(t, s, n)
		s.partialDelete = func() error { return errStop }
		must.ErrorIs(t, s.DeleteRange(1000, n), errStop)
		first, last := contiguous(t, s)
		must.Eq(t, uint64(1), first)
		must.Less(t, uint64(n), last)
		must.GreaterEq(t, uint64(999), last)
	})

	t.Run("tail up to the largest index", func(t *testing.T) {
		s := open(t, t.TempDir())
		defer s.Close()
		fill(t, s, 100)
		must.NoError(t, s.DeleteRange(51, math.MaxUint64))
		first, last := contiguous(t, s)
		must.Eq(t, uint64(1), first)
		must.Eq(t, uint64(50), last)
	})

	t.Run("head", func(t *testing.T) {
		s := open(t, t.TempDir())
		defer s.Close()
		fill(t, s, n)
		s.partialDelete = func() error { return errStop }
		must.ErrorIs(t, s.DeleteRange(1, n-1000), errStop)
		first, last := contiguous(t, s)
		must.Greater(t, uint64(1), first)
		must.Eq(t, uint64(n), last)

		s.partialDelete = nil
		must.NoError(t, s.DeleteRange(1, n-1000))
		first, _ = contiguous(t, s)
		must.Eq(t, uint64(n-999), first)
	})
}
