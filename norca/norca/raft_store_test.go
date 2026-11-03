// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/raftbadger"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/testutil"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/shoenig/test/must"
)

// TestServer_RaftStore_BoltDB asserts a server with raft_store = "boltdb"
// keeps its raft log in raft.db and works.
func TestServer_RaftStore_BoltDB(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()

	s, cleanup := TestServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = dir
		c.RaftStore = RaftStoreBoltDB
		c.RaftBoltNoFreelistSync = true
	})
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	_, ok := s.raftStore.(*raftboltdb.BoltStore)
	must.True(t, ok)
	raftDir := filepath.Join(dir, raftState)
	_, err := os.Stat(filepath.Join(raftDir, raftBoltFile))
	must.NoError(t, err)
	must.False(t, raftbadger.IsStore(filepath.Join(raftDir, raftBadgerDir)))

	last, err := s.raftStore.LastIndex()
	must.NoError(t, err)
	must.Positive(t, last)
}

// TestServer_RaftStore_otherStoreData asserts a server does not start next
// to the raft log of the other store.
func TestServer_RaftStore_otherStoreData(t *testing.T) {
	ci.Parallel(t)

	raftDir := t.TempDir()
	b, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(raftDir, raftBoltFile)})
	must.NoError(t, err)
	must.NoError(t, b.Close())
	s := &Server{config: &Config{}, logger: testlog.HCLogger(t)}
	_, err = s.openRaftStore(raftDir)
	must.ErrorContains(t, err, `set raft_store = "boltdb"`)

	raftDir = t.TempDir()
	r, err := raftbadger.Open(raftbadger.Options{Path: filepath.Join(raftDir, raftBadgerDir), MemTableSizeMB: 4})
	must.NoError(t, err)
	must.NoError(t, r.Close())
	s.config.RaftStore = RaftStoreBoltDB
	_, err = s.openRaftStore(raftDir)
	must.ErrorContains(t, err, `set raft_store = "badger"`)

	must.NoError(t, ValidateRaftStore(""))
	must.NoError(t, ValidateRaftStore(RaftStoreBoltDB))
	must.ErrorContains(t, ValidateRaftStore("etcd"), "unknown server raft_store")
}
