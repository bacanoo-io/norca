// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func secretStoreBlobPaths(t *testing.T, store *StateStore, prefix string) []string {
	t.Helper()
	iter, err := store.SecretStoreBlobsByPrefix(nil, prefix)
	must.NoError(t, err)
	var paths []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		paths = append(paths, raw.(*structs.SecretStoreBlob).Path)
	}
	return paths
}

func TestStateStore_SecretStoreBlobs(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	ws := memdb.NewWatchSet()
	blob, err := store.SecretStoreBlobByPath(ws, "core/keyring")
	must.NoError(t, err)
	must.Nil(t, blob)

	must.NoError(t, store.UpsertSecretStoreBlobs(structs.MsgTypeTestSetup, 10, []*structs.SecretStoreBlob{
		{Path: "core/keyring", Value: []byte("ciphertext-1")},
		{Path: "secret/data/app/db", Value: []byte("ciphertext-2")},
		{Path: "secret/metadata/app/db", Value: []byte("ciphertext-3")},
		nil,
	}))
	must.True(t, watchFired(ws))

	blob, err = store.SecretStoreBlobByPath(nil, "core/keyring")
	must.NoError(t, err)
	must.Eq(t, []byte("ciphertext-1"), blob.Value)
	must.Eq(t, 10, blob.CreateIndex)
	must.Eq(t, 10, blob.ModifyIndex)

	idx, err := store.Index(TableSecretStoreBlobs)
	must.NoError(t, err)
	must.Eq(t, 10, idx)

	must.Eq(t, []string{"core/keyring", "secret/data/app/db", "secret/metadata/app/db"}, secretStoreBlobPaths(t, store, ""))
	must.Eq(t, []string{"secret/data/app/db", "secret/metadata/app/db"}, secretStoreBlobPaths(t, store, "secret/"))
	must.Nil(t, secretStoreBlobPaths(t, store, "nothing/"))

	// An update keeps the create index.
	must.NoError(t, store.UpsertSecretStoreBlobs(structs.MsgTypeTestSetup, 20, []*structs.SecretStoreBlob{
		{Path: "core/keyring", Value: []byte("ciphertext-4")},
	}))
	blob, err = store.SecretStoreBlobByPath(nil, "core/keyring")
	must.NoError(t, err)
	must.Eq(t, []byte("ciphertext-4"), blob.Value)
	must.Eq(t, 10, blob.CreateIndex)
	must.Eq(t, 20, blob.ModifyIndex)

	// A blob needs a path.
	must.Error(t, store.UpsertSecretStoreBlobs(structs.MsgTypeTestSetup, 25, []*structs.SecretStoreBlob{{Value: []byte("x")}}))

	// Deleting ignores paths without a blob.
	ws = memdb.NewWatchSet()
	_, err = store.SecretStoreBlobByPath(ws, "secret/data/app/db")
	must.NoError(t, err)
	must.NoError(t, store.DeleteSecretStoreBlobs(structs.MsgTypeTestSetup, 30, []string{"secret/data/app/db", "missing"}))
	must.True(t, watchFired(ws))
	must.Eq(t, []string{"core/keyring", "secret/metadata/app/db"}, secretStoreBlobPaths(t, store, ""))

	idx, err = store.Index(TableSecretStoreBlobs)
	must.NoError(t, err)
	must.Eq(t, 30, idx)
}

func TestStateStore_SecretStoreBlobsNoEvents(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	blob := &structs.SecretStoreBlob{Path: "core/keyring", Value: []byte("ciphertext")}
	for _, msgType := range []structs.MessageType{
		structs.SecretStoreBlobsUpsertRequestType,
		structs.SecretStoreBlobsDeleteRequestType,
	} {
		changes := Changes{
			Index:   100,
			MsgType: msgType,
			Changes: memdb.Changes{{Table: TableSecretStoreBlobs, Before: blob, After: blob}},
		}
		out := eventsFromChanges(s.db.ReadTxn(), changes)
		if out != nil {
			must.SliceEmpty(t, out.Events)
		}
	}
}

func TestStateStore_SecretStoreBlobRestore(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	restore, err := store.Restore()
	must.NoError(t, err)
	blob := &structs.SecretStoreBlob{Path: "core/keyring", Value: []byte("ciphertext"), CreateIndex: 3, ModifyIndex: 4}
	must.NoError(t, restore.SecretStoreBlobRestore(blob))
	must.NoError(t, restore.Commit())

	out, err := store.SecretStoreBlobByPath(nil, "core/keyring")
	must.NoError(t, err)
	must.Eq(t, blob, out)
}
