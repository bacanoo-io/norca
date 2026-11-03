// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package raftutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/raftbadger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRaftStateInfo_InUse asserts that commands that inspect raft
// state such as "norca operator raft info" and "norca operator raft
// logs" fail with a helpful error message when called on a store that a
// server holds.
func TestRaftStateInfo_InUse(t *testing.T) {
	ci.Parallel(t)

	// First create an empty raft store
	dir := filepath.Join(t.TempDir(), raftStoreDir)

	fakedb, err := raftbadger.Open(raftbadger.Options{Path: dir, MemTableSizeMB: 4})
	require.NoError(t, err)

	// Next try to read the store without closing it
	s, _, _, err := RaftStateInfo(dir)
	assert.Nil(t, s)
	require.EqualError(t, err, errAlreadyOpen.Error())

	// LogEntries should produce the same error
	_, _, err = LogEntries(dir)
	require.EqualError(t, err, "failed to open raft logs: "+errAlreadyOpen.Error())

	// Commands should work once the store is closed
	require.NoError(t, fakedb.Close())

	s, _, _, err = RaftStateInfo(dir)
	require.NoError(t, err)
	assert.NotNil(t, s)
	require.NoError(t, s.Close())

	logCh, errCh, err := LogEntries(dir)
	require.NoError(t, err)

	// Consume entries and warnings until the reader closes the store
	for open := true; open; {
		select {
		case _, open = <-logCh:
		case <-errCh:
		}
	}
}

// TestFindRaftFile finds the Badger store from a data directory, a raft
// directory or the store itself, and reports the BoltDB file of an older
// Norca instead of reading it.
func TestFindRaftFile(t *testing.T) {
	ci.Parallel(t)

	data := t.TempDir()
	store := filepath.Join(data, "server", "raft", raftStoreDir)
	s, err := raftbadger.Open(raftbadger.Options{Path: store, MemTableSizeMB: 4})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	for _, p := range []string{data, filepath.Join(data, "server", "raft"), store} {
		got, err := FindRaftFile(p)
		require.NoError(t, err, p)
		require.Equal(t, store, got, p)
	}
	dir, err := FindRaftDir(data)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(data, "server", "raft"), dir)

	old := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(old, "server", "raft"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "server", "raft", "raft.db"), []byte("bolt"), 0o600))
	_, err = FindRaftFile(old)
	require.ErrorContains(t, err, "BoltDB raft store of an older Norca")

	_, err = FindRaftFile(t.TempDir())
	require.ErrorContains(t, err, "no raft store found")
}
