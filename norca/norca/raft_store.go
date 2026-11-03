// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bacanoo-io/norca/norca/helper/raftbadger"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

// The stores of the raft log (server { raft_store = ... }).
const (
	RaftStoreBadger = "badger"
	RaftStoreBoltDB = "boltdb"

	// raftBadgerDir and raftBoltFile are the stores inside the raft
	// directory of a server.
	raftBadgerDir = "badger"
	raftBoltFile  = "raft.db"
)

// raftLogStore is the raft log and stable store of a server.
type raftLogStore interface {
	raft.LogStore
	raft.StableStore
	Close() error
}

// ValidateRaftStore checks a raft_store value; empty is the default
// (Badger).
func ValidateRaftStore(store string) error {
	switch store {
	case "", RaftStoreBadger, RaftStoreBoltDB:
		return nil
	}
	return fmt.Errorf("unknown server raft_store %q (badger or boltdb)", store)
}

// openRaftStore opens the configured store in the raft directory. A server
// does not start next to the data of the other store: it would start with an
// empty log and lose its place in the cluster without notice.
func (s *Server) openRaftStore(raftDir string) (raftLogStore, error) {
	badgerPath := filepath.Join(raftDir, raftBadgerDir)
	boltPath := filepath.Join(raftDir, raftBoltFile)
	_, boltErr := os.Stat(boltPath)
	hasBolt := boltErr == nil
	hasBadger := raftbadger.IsStore(badgerPath)

	switch s.config.RaftStore {
	case RaftStoreBoltDB:
		if hasBadger && !hasBolt {
			return nil, fmt.Errorf("%s holds a Badger raft log and raft_store is %q: set raft_store = %q, "+
				"or remove the server's raft directory and let the server rejoin the cluster",
				raftDir, RaftStoreBoltDB, RaftStoreBadger)
		}
		store, err := raftboltdb.New(raftboltdb.Options{
			Path:   boltPath,
			NoSync: false, // fsync each log write
			BoltOptions: &bbolt.Options{
				NoFreelistSync: s.config.RaftBoltNoFreelistSync,
			},
		})
		if err != nil {
			return nil, err
		}
		s.logger.Info("setting up raft bolt store", "path", boltPath,
			"no_freelist_sync", s.config.RaftBoltNoFreelistSync)
		// publish the bbolt metrics (nomad.raft.boltdb.*)
		go store.RunMetrics(s.shutdownCtx, 0)
		return store, nil

	default:
		if hasBolt && !hasBadger {
			return nil, fmt.Errorf("%s holds a BoltDB raft log and raft_store is %q: set raft_store = %q, "+
				"or remove the server's raft directory and let the server rejoin the cluster",
				raftDir, RaftStoreBadger, RaftStoreBoltDB)
		}
		opts := s.config.RaftBadger
		opts.Path = badgerPath
		opts.Logger = s.logger.Named("raft-store")
		store, err := raftbadger.Open(opts)
		if err != nil {
			return nil, err
		}
		s.logger.Info("setting up raft badger store", "path", opts.Path)
		return store, nil
	}
}
