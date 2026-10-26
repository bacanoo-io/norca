// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TableSecretStoreBlobs is the table of the physical storage of the built-in
// secret store: the blobs written by the uBixVault barrier, keyed by storage
// path. The values are stored as they are written (ciphertext); the state
// store never interprets them, and changes to the table produce no events.
const TableSecretStoreBlobs = "secret_store_blobs"

func init() {
	RegisterSchemaFactories(secretStoreBlobsTableSchema)
}

// secretStoreBlobsTableSchema returns the MemDB schema of the secret store
// blobs table. The ID index is the storage path; its prefix form lists a
// subtree.
func secretStoreBlobsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableSecretStoreBlobs,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer:      &memdb.StringFieldIndex{Field: "Path"},
			},
		},
	}
}

// SecretStoreBlobByPath returns the secret store blob at path, or nil. The
// returned blob is the state store's own copy and must not be modified.
func (s *StateStore) SecretStoreBlobByPath(ws memdb.WatchSet, path string) (*structs.SecretStoreBlob, error) {
	txn := s.db.ReadTxn()

	watchCh, existing, err := txn.FirstWatch(TableSecretStoreBlobs, indexID, path)
	if err != nil {
		return nil, fmt.Errorf("secret store blob lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.SecretStoreBlob), nil
}

// SecretStoreBlobsByPrefix returns an iterator over the secret store blobs
// whose path starts with prefix, ordered by path. An empty prefix returns
// all blobs.
func (s *StateStore) SecretStoreBlobsByPrefix(ws memdb.WatchSet, prefix string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	iter, err := txn.Get(TableSecretStoreBlobs, indexID+"_prefix", prefix)
	if err != nil {
		return nil, fmt.Errorf("secret store blob lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// UpsertSecretStoreBlobs inserts or replaces secret store blobs.
func (s *StateStore) UpsertSecretStoreBlobs(msgType structs.MessageType, index uint64, blobs []*structs.SecretStoreBlob) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, blob := range blobs {
		if blob == nil {
			continue
		}
		if blob.Path == "" {
			return fmt.Errorf("secret store blob without a path")
		}
		existing, err := txn.First(TableSecretStoreBlobs, indexID, blob.Path)
		if err != nil {
			return fmt.Errorf("secret store blob lookup failed: %w", err)
		}
		if existing != nil {
			blob.CreateIndex = existing.(*structs.SecretStoreBlob).CreateIndex
		} else {
			blob.CreateIndex = index
		}
		blob.ModifyIndex = index

		if err := txn.Insert(TableSecretStoreBlobs, blob); err != nil {
			return fmt.Errorf("secret store blob insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableSecretStoreBlobs, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteSecretStoreBlobs removes the secret store blobs at the given paths.
// Paths without a blob are ignored, as the storage interface of the secret
// store requires.
func (s *StateStore) DeleteSecretStoreBlobs(msgType structs.MessageType, index uint64, paths []string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, path := range paths {
		existing, err := txn.First(TableSecretStoreBlobs, indexID, path)
		if err != nil {
			return fmt.Errorf("secret store blob lookup failed: %w", err)
		}
		if existing == nil {
			continue
		}
		if err := txn.Delete(TableSecretStoreBlobs, existing); err != nil {
			return fmt.Errorf("secret store blob deletion failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableSecretStoreBlobs, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// SecretStoreBlobRestore is used to restore a secret store blob from a
// snapshot.
func (r *StateRestore) SecretStoreBlobRestore(blob *structs.SecretStoreBlob) error {
	if err := r.txn.Insert(TableSecretStoreBlobs, blob); err != nil {
		return fmt.Errorf("secret store blob insert failed: %w", err)
	}
	return nil
}
