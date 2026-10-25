// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/raft"
)

// Snapshot types of the backup tables. They use the numbers of the Raft
// message types assigned to backups (79 is reserved).
const (
	BackupSnapshot        SnapshotType = 78
	BackupRestoreSnapshot SnapshotType = 80
)

func (n *nomadFSM) applyBackupUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_backup_upsert"}, time.Now())
	var req structs.BackupUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertBackups(msgType, index, req.Backups); err != nil {
		n.logger.Error("UpsertBackups failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyBackupDelete(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_backup_delete"}, time.Now())
	var req structs.BackupDeleteRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.DeleteBackups(msgType, index, req.IDs); err != nil {
		n.logger.Error("DeleteBackups failed", "error", err)
		return err
	}
	return nil
}

func (n *nomadFSM) applyBackupRestoreUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"nomad", "fsm", "apply_backup_restore_upsert"}, time.Now())
	var req structs.BackupRestoreUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertBackupRestores(msgType, index, req.Restores); err != nil {
		n.logger.Error("UpsertBackupRestores failed", "error", err)
		return err
	}
	return nil
}

// restoreBackupSnapshot restores a backup or restore record.
func restoreBackupSnapshot(snapType SnapshotType, restore *state.StateRestore, dec *codec.Decoder) error {
	switch snapType {
	case BackupSnapshot:
		backup := new(structs.BackupRecord)
		if err := dec.Decode(backup); err != nil {
			return err
		}
		return restore.BackupRestore(backup)
	case BackupRestoreSnapshot:
		record := new(structs.BackupRestore)
		if err := dec.Decode(record); err != nil {
			return err
		}
		return restore.BackupRestoreRecordRestore(record)
	}
	return fmt.Errorf("unexpected backup snapshot type %d", snapType)
}

// persistBackups persists the backup and restore records.
func (s *nomadSnapshot) persistBackups(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	ws := memdb.NewWatchSet()
	backups, err := s.snap.Backups(ws, state.SortDefault)
	if err != nil {
		return err
	}
	for raw := backups.Next(); raw != nil; raw = backups.Next() {
		backup := raw.(*structs.BackupRecord)
		sink.Write([]byte{byte(BackupSnapshot)})
		if err := encoder.Encode(backup); err != nil {
			return err
		}
	}

	restores, err := s.snap.BackupRestores(ws)
	if err != nil {
		return err
	}
	for raw := restores.Next(); raw != nil; raw = restores.Next() {
		record := raw.(*structs.BackupRestore)
		sink.Write([]byte{byte(BackupRestoreSnapshot)})
		if err := encoder.Encode(record); err != nil {
			return err
		}
	}
	return nil
}
