// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TableSecrets is the table of the metadata rows of the built-in secret
// store's secrets. The rows never contain values: those live, encrypted, in
// the secret_store_blobs table, written in the same transaction as the row.
const TableSecrets = "secrets"

func init() {
	RegisterSchemaFactories(secretsTableSchema)
}

// secretsTableSchema returns the MemDB schema of the secrets table. The ID
// index is the namespace and the path; its prefix form lists the secrets of a
// namespace below a path prefix.
func secretsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableSecrets,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.CompoundIndex{
					Indexes: []memdb.Indexer{
						&memdb.StringFieldIndex{Field: "Namespace"},
						&memdb.StringFieldIndex{Field: "Path"},
					},
				},
			},
		},
	}
}

// Secrets returns an iterator over the secrets of all namespaces, ordered by
// namespace and path.
func (s *StateStore) Secrets(ws memdb.WatchSet) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	iter, err := txn.Get(TableSecrets, indexID)
	if err != nil {
		return nil, fmt.Errorf("secrets lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// SecretsByNamespacePrefix returns an iterator over the secrets of a
// namespace whose path starts with prefix, ordered by path.
func (s *StateStore) SecretsByNamespacePrefix(ws memdb.WatchSet, namespace, prefix string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	iter, err := txn.Get(TableSecrets, indexID+"_prefix", namespace, prefix)
	if err != nil {
		return nil, fmt.Errorf("secrets lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// SecretByPath returns the secret of the namespace at path, or nil. The
// returned secret is the state store's own copy and must not be modified.
func (s *StateStore) SecretByPath(ws memdb.WatchSet, namespace, path string) (*structs.Secret, error) {
	txn := s.db.ReadTxn()

	watchCh, existing, err := txn.FirstWatch(TableSecrets, indexID, namespace, path)
	if err != nil {
		return nil, fmt.Errorf("secret lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.Secret), nil
}

// ApplySecret writes a secret's metadata row and the barrier blobs of its
// versions in one transaction. It fails with structs.ErrSecretConflict,
// without changing anything, if the row's ModifyIndex is not
// req.ExpectedIndex (0: the row must not exist) or if a blob of
// req.CreateBlobs exists already.
func (s *StateStore) ApplySecret(msgType structs.MessageType, index uint64, req *structs.SecretApplyRequest) error {
	if req.Secret == nil {
		return fmt.Errorf("secret apply without a secret")
	}
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	secret := req.Secret
	existing, err := secretCheckIndexTxn(txn, secret.Namespace, secret.Path, req.ExpectedIndex)
	if err != nil {
		return err
	}
	for _, path := range req.CreateBlobs {
		blob, err := txn.First(TableSecretStoreBlobs, indexID, path)
		if err != nil {
			return fmt.Errorf("secret store blob lookup failed: %w", err)
		}
		if blob != nil {
			return structs.ErrSecretConflict
		}
	}

	if err := upsertSecretStoreBlobsTxn(txn, index, req.Blobs); err != nil {
		return err
	}
	if err := deleteSecretStoreBlobsTxn(txn, index, req.DeleteBlobs); err != nil {
		return err
	}

	if existing != nil {
		secret.CreateIndex = existing.CreateIndex
		secret.CreateTime = existing.CreateTime
	} else {
		secret.CreateIndex = index
	}
	secret.ModifyIndex = index
	if err := txn.Insert(TableSecrets, secret); err != nil {
		return fmt.Errorf("secret insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSecrets, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// PurgeSecret removes a secret's metadata row and the barrier blobs of its
// versions in one transaction. It fails with structs.ErrSecretConflict if
// the row's ModifyIndex is not req.ExpectedIndex; a missing row with an
// expected index of 0 is not an error.
func (s *StateStore) PurgeSecret(msgType structs.MessageType, index uint64, req *structs.SecretPurgeRequest) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	existing, err := secretCheckIndexTxn(txn, req.Namespace, req.Path, req.ExpectedIndex)
	if err != nil {
		return err
	}
	if err := deleteSecretStoreBlobsTxn(txn, index, req.DeleteBlobs); err != nil {
		return err
	}
	if existing != nil {
		if err := txn.Delete(TableSecrets, existing); err != nil {
			return fmt.Errorf("secret deletion failed: %w", err)
		}
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSecrets, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// secretCheckIndexTxn returns the existing row, or a conflict if its
// ModifyIndex is not expected.
func secretCheckIndexTxn(txn *txn, namespace, path string, expected uint64) (*structs.Secret, error) {
	raw, err := txn.First(TableSecrets, indexID, namespace, path)
	if err != nil {
		return nil, fmt.Errorf("secret lookup failed: %w", err)
	}
	var existing *structs.Secret
	var current uint64
	if raw != nil {
		existing = raw.(*structs.Secret)
		current = existing.ModifyIndex
	}
	if current != expected {
		return nil, structs.ErrSecretConflict
	}
	return existing, nil
}

// upsertSecretStoreBlobsTxn writes barrier blobs in a transaction.
func upsertSecretStoreBlobsTxn(txn *txn, index uint64, blobs []*structs.SecretStoreBlob) error {
	if len(blobs) == 0 {
		return nil
	}
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
	return nil
}

// deleteSecretStoreBlobsTxn removes barrier blobs in a transaction; missing
// paths are ignored.
func deleteSecretStoreBlobsTxn(txn *txn, index uint64, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
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
	return nil
}

// SecretRestore is used to restore a secret from a snapshot.
func (r *StateRestore) SecretRestore(secret *structs.Secret) error {
	if err := r.txn.Insert(TableSecrets, secret); err != nil {
		return fmt.Errorf("secret insert failed: %w", err)
	}
	return nil
}

// secretEventFromChange builds the Secret topic event of a change of the
// secrets table: the metadata row, which never contains values.
func secretEventFromChange(change memdb.Change) (structs.Event, bool) {
	if change.Deleted() {
		before, ok := change.Before.(*structs.Secret)
		if !ok {
			return structs.Event{}, false
		}
		return structs.Event{
			Topic:     structs.TopicSecret,
			Type:      structs.TypeSecretDeleted,
			Key:       before.Path,
			Namespace: before.Namespace,
			Payload:   &structs.SecretEvent{Secret: before.Copy()},
		}, true
	}
	after, ok := change.After.(*structs.Secret)
	if !ok {
		return structs.Event{}, false
	}
	return structs.Event{
		Topic:     structs.TopicSecret,
		Type:      structs.TypeSecretUpserted,
		Key:       after.Path,
		Namespace: after.Namespace,
		Payload:   &structs.SecretEvent{Secret: after.Copy()},
	}, true
}
