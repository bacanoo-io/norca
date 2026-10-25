// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"
	"slices"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// TableBackups is the backup_status table: the records of the backups
	// of the region.
	TableBackups = "backup_status"

	// TableBackupRestores holds the records of restores from backups.
	TableBackupRestores = "backup_restores"
)

func init() {
	RegisterSchemaFactories(backupsTableSchema, backupRestoresTableSchema)
}

// backupsTableSchema returns the MemDB schema of the backup_status table.
func backupsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableBackups,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field: "ID",
				},
			},
		},
	}
}

// backupRestoresTableSchema returns the MemDB schema of the restores table.
func backupRestoresTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableBackupRestores,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field: "ID",
				},
			},
		},
	}
}

// Backups returns an iterator over the backup records. Backup IDs start with
// their UTC time, so the default order is oldest first.
func (s *StateStore) Backups(ws memdb.WatchSet, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableBackups, indexID)
	default:
		iter, err = txn.Get(TableBackups, indexID)
	}
	if err != nil {
		return nil, fmt.Errorf("backups lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// BackupsList returns all backup records, newest first.
func (s *StateStore) BackupsList(ws memdb.WatchSet) ([]*structs.BackupRecord, error) {
	iter, err := s.Backups(ws, SortReverse)
	if err != nil {
		return nil, err
	}
	var out []*structs.BackupRecord
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		out = append(out, raw.(*structs.BackupRecord))
	}
	return out, nil
}

// BackupByID returns the backup record with the given ID or nil.
func (s *StateStore) BackupByID(ws memdb.WatchSet, id string) (*structs.BackupRecord, error) {
	txn := s.db.ReadTxn()

	watchCh, existing, err := txn.FirstWatch(TableBackups, indexID, id)
	if err != nil {
		return nil, fmt.Errorf("backup lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.BackupRecord), nil
}

// UpsertBackups inserts or updates backup records.
func (s *StateStore) UpsertBackups(msgType structs.MessageType, index uint64, backups []*structs.BackupRecord) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, backup := range backups {
		if backup == nil {
			continue
		}
		existing, err := txn.First(TableBackups, indexID, backup.ID)
		if err != nil {
			return fmt.Errorf("backup lookup failed: %w", err)
		}
		if existing != nil {
			backup.CreateIndex = existing.(*structs.BackupRecord).CreateIndex
		} else {
			backup.CreateIndex = index
		}
		backup.ModifyIndex = index

		if err := txn.Insert(TableBackups, backup); err != nil {
			return fmt.Errorf("backup insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableBackups, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteBackups removes the backup records with the given IDs. Missing
// records are ignored, so retention running on a new leader is idempotent.
func (s *StateStore) DeleteBackups(msgType structs.MessageType, index uint64, ids []string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, id := range ids {
		existing, err := txn.First(TableBackups, indexID, id)
		if err != nil {
			return fmt.Errorf("backup lookup failed: %w", err)
		}
		if existing == nil {
			continue
		}
		if err := txn.Delete(TableBackups, existing); err != nil {
			return fmt.Errorf("backup deletion failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableBackups, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// BackupRestore is used to restore a backup record from a snapshot.
func (r *StateRestore) BackupRestore(backup *structs.BackupRecord) error {
	if err := r.txn.Insert(TableBackups, backup); err != nil {
		return fmt.Errorf("backup insert failed: %w", err)
	}
	return nil
}

// BackupRestores returns an iterator over the restore records.
func (s *StateStore) BackupRestores(ws memdb.WatchSet) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	iter, err := txn.Get(TableBackupRestores, indexID)
	if err != nil {
		return nil, fmt.Errorf("backup restores lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// BackupRestoresList returns all restore records, newest first.
func (s *StateStore) BackupRestoresList(ws memdb.WatchSet) ([]*structs.BackupRestore, error) {
	iter, err := s.BackupRestores(ws)
	if err != nil {
		return nil, err
	}
	var out []*structs.BackupRestore
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		out = append(out, raw.(*structs.BackupRestore))
	}
	slices.SortStableFunc(out, func(a, b *structs.BackupRestore) int {
		return b.CreateTime.Compare(a.CreateTime)
	})
	return out, nil
}

// BackupRestoreByID returns the restore record with the given ID or nil.
func (s *StateStore) BackupRestoreByID(ws memdb.WatchSet, id string) (*structs.BackupRestore, error) {
	txn := s.db.ReadTxn()

	watchCh, existing, err := txn.FirstWatch(TableBackupRestores, indexID, id)
	if err != nil {
		return nil, fmt.Errorf("backup restore lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.BackupRestore), nil
}

// UpsertBackupRestores inserts or updates restore records.
func (s *StateStore) UpsertBackupRestores(msgType structs.MessageType, index uint64, restores []*structs.BackupRestore) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, restore := range restores {
		if restore == nil {
			continue
		}
		existing, err := txn.First(TableBackupRestores, indexID, restore.ID)
		if err != nil {
			return fmt.Errorf("backup restore lookup failed: %w", err)
		}
		if existing != nil {
			restore.CreateIndex = existing.(*structs.BackupRestore).CreateIndex
		} else {
			restore.CreateIndex = index
		}
		restore.ModifyIndex = index

		if err := txn.Insert(TableBackupRestores, restore); err != nil {
			return fmt.Errorf("backup restore insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableBackupRestores, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// BackupRestoreRecordRestore is used to restore a restore record from a
// snapshot.
func (r *StateRestore) BackupRestoreRecordRestore(restore *structs.BackupRestore) error {
	if err := r.txn.Insert(TableBackupRestores, restore); err != nil {
		return fmt.Errorf("backup restore insert failed: %w", err)
	}
	return nil
}

// backupEventFromChange returns the event stream event of a change in the
// backup tables. The event type follows the record, not the message type.
func backupEventFromChange(change memdb.Change) (structs.Event, bool) {
	switch change.Table {
	case TableBackups:
		if change.Deleted() {
			before, ok := change.Before.(*structs.BackupRecord)
			if !ok {
				return structs.Event{}, false
			}
			return structs.Event{
				Topic:   structs.TopicBackup,
				Type:    structs.TypeBackupDeleted,
				Key:     before.ID,
				Payload: &structs.BackupEvent{Backup: before},
			}, true
		}
		after, ok := change.After.(*structs.BackupRecord)
		if !ok {
			return structs.Event{}, false
		}
		eventType := structs.TypeBackupUpdated
		if change.Created() {
			eventType = structs.TypeBackupCompleted
			if after.Status == structs.BackupStatusFailed {
				eventType = structs.TypeBackupFailed
			}
		}
		return structs.Event{
			Topic:      structs.TopicBackup,
			Type:       eventType,
			Key:        after.ID,
			FilterKeys: []string{after.Status},
			Payload:    &structs.BackupEvent{Backup: after},
		}, true

	case TableBackupRestores:
		after, ok := change.After.(*structs.BackupRestore)
		if !ok {
			return structs.Event{}, false
		}
		var eventType string
		switch after.Status {
		case structs.BackupRestoreStatusPending:
			eventType = structs.TypeBackupRestoreRequested
		case structs.BackupRestoreStatusApproved:
			eventType = structs.TypeBackupRestoreApproved
		case structs.BackupRestoreStatusRunning:
			eventType = structs.TypeBackupRestoreStarted
		case structs.BackupRestoreStatusRejected, structs.BackupRestoreStatusExpired:
			eventType = structs.TypeBackupRestoreRejected
		default:
			eventType = structs.TypeBackupRestoreFinished
		}
		return structs.Event{
			Topic:      structs.TopicBackup,
			Type:       eventType,
			Key:        after.ID,
			FilterKeys: []string{after.BackupID},
			Payload:    &structs.BackupRestoreEvent{Restore: after},
		}, true
	}
	return structs.Event{}, false
}
