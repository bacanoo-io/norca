// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"
	"strconv"

	"github.com/hashicorp/nomad/helper/snapshot"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// setupBackup creates the backup manager. It only takes backups while this
// server is the leader.
func (s *Server) setupBackup() {
	cfg := s.config.Backup
	if cfg == nil {
		cfg = backup.DefaultConfig()
	}
	s.backup = backup.New(cfg, &backup.Deps{
		Logger:              s.logger,
		Region:              s.config.Region,
		AuthoritativeRegion: s.config.AuthoritativeRegion,
		LeaderID:            s.config.NodeName,
		ServersReady: func() bool {
			return ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false)
		},
		Snapshot:     s.backupSnapshot,
		Keyring:      s.backupKeyring,
		KeyringReady: func() bool { _, err := s.backupKeyring(); return err == nil },
		State:        func() *state.StateStore { return s.fsm.State() },
		Apply: func(msgType structs.MessageType, req any) (uint64, error) {
			_, index, err := s.raftApply(msgType, req)
			return index, err
		},
	})
}

// backupKeyring returns the root keys of the keyring. It fails until the
// active root key is in the local keyring.
func (s *Server) backupKeyring() ([]*structs.RootKey, error) {
	if s.encrypter == nil {
		return nil, errors.New("keyring is not initialized")
	}
	active, err := s.fsm.State().GetActiveRootKeyMeta(nil)
	if err != nil {
		return nil, err
	}
	if active == nil {
		return nil, errors.New("keyring is not initialized")
	}
	keys := s.encrypter.exportKeys()
	for _, k := range keys {
		if k.Meta.KeyID == active.KeyID {
			return keys, nil
		}
	}
	return nil, errors.New("active root key is not in the local keyring yet")
}

// backupSnapshot is a Raft snapshot taken for a backup.
type backupSnapshot struct {
	*snapshot.Snapshot
	term uint64
}

func (b *backupSnapshot) Term() uint64 { return b.term }

// backupSnapshot takes a Raft snapshot with the path snapshotSave uses.
func (s *Server) backupSnapshot() (backup.Snapshot, error) {
	snap, err := snapshot.New(s.logger.Named("snapshot"), s.raft)
	if err != nil {
		return nil, err
	}
	term, _ := strconv.ParseUint(s.raft.Stats()["term"], 10, 64)
	return &backupSnapshot{Snapshot: snap, term: term}, nil
}
