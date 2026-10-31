// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/hashicorp/nomad/helper/snapshot"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/seal"
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
		Snapshot:       s.backupSnapshot,
		Keyring:        s.backupKeyring,
		SecretStoreKey: s.backupSecretStoreKey,
		ConsulKV:       s.backupConsulKV,
		KeyringReady:   func() bool { _, err := s.backupKeyring(); return err == nil },
		State:          func() *state.StateStore { return s.fsm.State() },
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
	if s.encrypter.isSealed() {
		return nil, structs.ErrSealed
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

// backupSecretStoreKey returns the key of the secret store core, unwrapped
// with the seal of this server, for the secret store key object of a backup
// (Norca): a restore re-wraps it with the seals of the target region. It
// returns nil if the store is not initialized.
func (s *Server) backupSecretStoreKey() ([]byte, error) {
	if s.fsm == nil || s.seal == nil {
		return nil, nil
	}
	blob, err := s.fsm.State().SecretStoreBlobByPath(nil, seal.CoreMasterKeyPath)
	if err != nil || blob == nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(s.shutdownCtx, 10*time.Second)
	defer cancel()
	return (&seal.CoreSeal{Keyring: s.seal}).Unwrap(ctx, blob.Value)
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
