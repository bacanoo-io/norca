// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestStateStore_Backups(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStore(t)

	b1 := &structs.BackupRecord{ID: "20261024T130000Z-10", Status: structs.BackupStatusComplete}
	b2 := &structs.BackupRecord{ID: "20261024T140000Z-20", Status: structs.BackupStatusFailed}
	must.NoError(t, s.UpsertBackups(structs.MsgTypeTestSetup, 100, []*structs.BackupRecord{b1, b2}))

	list, err := s.BackupsList(nil)
	must.NoError(t, err)
	must.Len(t, 2, list)
	must.Eq(t, b2.ID, list[0].ID) // newest first

	updated := b1.Copy()
	updated.Pinned = true
	must.NoError(t, s.UpsertBackups(structs.MsgTypeTestSetup, 110, []*structs.BackupRecord{updated}))
	got, err := s.BackupByID(nil, b1.ID)
	must.NoError(t, err)
	must.True(t, got.Pinned)
	must.Eq(t, uint64(100), got.CreateIndex)
	must.Eq(t, uint64(110), got.ModifyIndex)

	// Deleting a missing record is not an error.
	must.NoError(t, s.DeleteBackups(structs.MsgTypeTestSetup, 120, []string{b1.ID, "missing"}))
	got, err = s.BackupByID(nil, b1.ID)
	must.NoError(t, err)
	must.Nil(t, got)

	index, err := s.Index(TableBackups)
	must.NoError(t, err)
	must.Eq(t, uint64(120), index)
}

func TestStateStore_BackupEvents(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStore(t)

	b := &structs.BackupRecord{ID: "20261024T130000Z-10", Status: structs.BackupStatusComplete}
	failed := &structs.BackupRecord{ID: "20261024T140000Z-20", Status: structs.BackupStatusFailed}
	must.NoError(t, s.UpsertBackups(structs.MsgTypeTestSetup, 100, []*structs.BackupRecord{b}))

	events := func(msgType structs.MessageType, index uint64, fn func(txn *txn) error) []structs.Event {
		txn := s.db.WriteTxnMsgT(msgType, index)
		defer txn.Abort()
		must.NoError(t, fn(txn))
		out := eventsFromChanges(txn, Changes{Index: index, Changes: txn.Changes(), MsgType: msgType})
		must.NotNil(t, out)
		return out.Events
	}

	evs := events(structs.BackupUpsertRequestType, 200, func(txn *txn) error {
		return txn.Insert(TableBackups, failed)
	})
	must.Len(t, 1, evs)
	must.Eq(t, structs.TopicBackup, evs[0].Topic)
	must.Eq(t, structs.TypeBackupFailed, evs[0].Type)

	pinned := b.Copy()
	pinned.Pinned = true
	evs = events(structs.BackupUpsertRequestType, 201, func(txn *txn) error {
		return txn.Insert(TableBackups, pinned)
	})
	must.Eq(t, structs.TypeBackupUpdated, evs[0].Type)

	evs = events(structs.BackupDeleteRequestType, 202, func(txn *txn) error {
		return txn.Delete(TableBackups, b)
	})
	must.Eq(t, structs.TypeBackupDeleted, evs[0].Type)

	evs = events(structs.BackupRestoreUpsertRequestType, 203, func(txn *txn) error {
		return txn.Insert(TableBackupRestores, &structs.BackupRestore{ID: "r", BackupID: b.ID,
			Status: structs.BackupRestoreStatusRunning})
	})
	must.Eq(t, structs.TypeBackupRestoreStarted, evs[0].Type)
	must.Eq(t, []string{b.ID}, evs[0].FilterKeys)
}
