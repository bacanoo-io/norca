// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// TableMTLSRoots holds the certificate authorities of the native mTLS
	// trust bundle.
	TableMTLSRoots = "mtls_roots"

	// TableServiceIntentions holds the service intentions.
	TableServiceIntentions = "service_intentions"

	indexSigningKeyID = "signing_key_id"
)

func init() {
	RegisterSchemaFactories(mtlsRootsTableSchema, serviceIntentionsTableSchema)

	// Trust bundle changes are not published; the clients watch the roots
	// with blocking queries.
	MsgTypeEvents[structs.ServiceIntentionUpsertRequestType] = structs.TypeServiceIntentionUpserted
	MsgTypeEvents[structs.ServiceIntentionDeleteRequestType] = structs.TypeServiceIntentionDeleted
}

// mtlsRootsTableSchema returns the MemDB schema of the mtls_roots table.
func mtlsRootsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableMTLSRoots,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field: "ID",
				},
			},
			indexSigningKeyID: {
				Name:         indexSigningKeyID,
				AllowMissing: true,
				Unique:       false,
				Indexer: &memdb.StringFieldIndex{
					Field: "SigningKeyID",
				},
			},
		},
	}
}

// serviceIntentionsTableSchema returns the MemDB schema of the
// service_intentions table. The ID index is the unique key of an intention:
// namespace, destination, source namespace and source.
func serviceIntentionsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableServiceIntentions,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.CompoundIndex{
					Indexes: []memdb.Indexer{
						&memdb.StringFieldIndex{Field: "Namespace"},
						&memdb.StringFieldIndex{Field: "Destination"},
						&memdb.StringFieldIndex{Field: "SourceNamespace"},
						&memdb.StringFieldIndex{Field: "Source"},
					},
				},
			},
			indexNamespace: {
				Name:         indexNamespace,
				AllowMissing: false,
				Unique:       false,
				Indexer: &memdb.StringFieldIndex{
					Field: "Namespace",
				},
			},
		},
	}
}

// MTLSRoots returns all roots of the trust bundle.
func (s *StateStore) MTLSRoots(ws memdb.WatchSet) ([]*structs.MTLSRoot, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableMTLSRoots, indexID)
	if err != nil {
		return nil, fmt.Errorf("mtls roots lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())

	var out []*structs.MTLSRoot
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		out = append(out, raw.(*structs.MTLSRoot))
	}
	return out, nil
}

// MTLSRootByID returns the root with the given ID or nil.
func (s *StateStore) MTLSRootByID(ws memdb.WatchSet, id string) (*structs.MTLSRoot, error) {
	txn := s.db.ReadTxn()
	watchCh, existing, err := txn.FirstWatch(TableMTLSRoots, indexID, id)
	if err != nil {
		return nil, fmt.Errorf("mtls root lookup failed: %w", err)
	}
	ws.Add(watchCh)
	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.MTLSRoot), nil
}

// UpsertMTLSRoots inserts or updates roots and deletes the roots with the
// given IDs, in one transaction.
func (s *StateStore) UpsertMTLSRoots(msgType structs.MessageType, index uint64, roots []*structs.MTLSRoot, deleteIDs []string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, id := range deleteIDs {
		existing, err := txn.First(TableMTLSRoots, indexID, id)
		if err != nil {
			return fmt.Errorf("mtls root lookup failed: %w", err)
		}
		if existing == nil {
			continue
		}
		if err := txn.Delete(TableMTLSRoots, existing); err != nil {
			return fmt.Errorf("mtls root deletion failed: %w", err)
		}
	}

	for _, root := range roots {
		if root == nil {
			continue
		}
		existing, err := txn.First(TableMTLSRoots, indexID, root.ID)
		if err != nil {
			return fmt.Errorf("mtls root lookup failed: %w", err)
		}
		root = root.Copy()
		if existing != nil {
			root.CreateIndex = existing.(*structs.MTLSRoot).CreateIndex
		} else {
			root.CreateIndex = index
		}
		root.ModifyIndex = index
		if err := txn.Insert(TableMTLSRoots, root); err != nil {
			return fmt.Errorf("mtls root insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableMTLSRoots, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// MTLSRootRestore is used to restore a root from a snapshot.
func (r *StateRestore) MTLSRootRestore(root *structs.MTLSRoot) error {
	if err := r.txn.Insert(TableMTLSRoots, root); err != nil {
		return fmt.Errorf("mtls root insert failed: %w", err)
	}
	return nil
}

// isRootKeyInUseByMTLS returns whether a root key of the keyring encrypts
// the private key of a certificate authority.
func isRootKeyInUseByMTLS(txn ReadTxn, keyID string) (bool, error) {
	raw, err := txn.First(TableMTLSRoots, indexSigningKeyID, keyID)
	if err != nil {
		return false, fmt.Errorf("mtls root lookup failed: %w", err)
	}
	return raw != nil, nil
}

// ServiceIntentions returns the intentions of a namespace, or of every
// namespace for the "*" namespace.
func (s *StateStore) ServiceIntentions(ws memdb.WatchSet, namespace string) ([]*structs.ServiceIntention, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	if namespace == structs.AllNamespacesSentinel {
		iter, err = txn.Get(TableServiceIntentions, indexID)
	} else {
		iter, err = txn.Get(TableServiceIntentions, indexNamespace, namespace)
	}
	if err != nil {
		return nil, fmt.Errorf("service intentions lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())

	var out []*structs.ServiceIntention
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		out = append(out, raw.(*structs.ServiceIntention))
	}
	return out, nil
}

// ServiceIntentionByKey returns the intention with the given key or nil.
func (s *StateStore) ServiceIntentionByKey(ws memdb.WatchSet, namespace, destination, sourceNamespace, source string) (*structs.ServiceIntention, error) {
	txn := s.db.ReadTxn()
	watchCh, existing, err := txn.FirstWatch(TableServiceIntentions, indexID, namespace, destination, sourceNamespace, source)
	if err != nil {
		return nil, fmt.Errorf("service intention lookup failed: %w", err)
	}
	ws.Add(watchCh)
	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.ServiceIntention), nil
}

// UpsertServiceIntentions creates or replaces intentions.
func (s *StateStore) UpsertServiceIntentions(msgType structs.MessageType, index uint64, intentions []*structs.ServiceIntention) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, intention := range intentions {
		if intention == nil {
			continue
		}
		intention = intention.Copy()
		existing, err := txn.First(TableServiceIntentions, indexID,
			intention.Namespace, intention.Destination, intention.SourceNamespace, intention.Source)
		if err != nil {
			return fmt.Errorf("service intention lookup failed: %w", err)
		}
		if existing != nil {
			intention.CreateIndex = existing.(*structs.ServiceIntention).CreateIndex
		} else {
			intention.CreateIndex = index
		}
		intention.ModifyIndex = index
		if err := txn.Insert(TableServiceIntentions, intention); err != nil {
			return fmt.Errorf("service intention insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableServiceIntentions, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteServiceIntentions deletes intentions by key. Missing intentions are
// ignored.
func (s *StateStore) DeleteServiceIntentions(msgType structs.MessageType, index uint64, keys []*structs.ServiceIntention) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, key := range keys {
		if key == nil {
			continue
		}
		existing, err := txn.First(TableServiceIntentions, indexID,
			key.Namespace, key.Destination, key.SourceNamespace, key.Source)
		if err != nil {
			return fmt.Errorf("service intention lookup failed: %w", err)
		}
		if existing == nil {
			continue
		}
		if err := txn.Delete(TableServiceIntentions, existing); err != nil {
			return fmt.Errorf("service intention deletion failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableServiceIntentions, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// ServiceIntentionRestore is used to restore an intention from a snapshot.
func (r *StateRestore) ServiceIntentionRestore(intention *structs.ServiceIntention) error {
	if err := r.txn.Insert(TableServiceIntentions, intention); err != nil {
		return fmt.Errorf("service intention insert failed: %w", err)
	}
	return nil
}

// serviceIntentionEventFromChange returns the event stream event of a change
// of the service intentions table.
func serviceIntentionEventFromChange(change memdb.Change) (structs.Event, bool) {
	eventType := structs.TypeServiceIntentionUpserted
	obj := change.After
	if change.Deleted() {
		eventType = structs.TypeServiceIntentionDeleted
		obj = change.Before
	}
	intention, ok := obj.(*structs.ServiceIntention)
	if !ok {
		return structs.Event{}, false
	}
	return structs.Event{
		Topic:      structs.TopicServiceIntention,
		Type:       eventType,
		Key:        intention.Destination,
		Namespace:  intention.Namespace,
		FilterKeys: []string{intention.Source},
		Payload:    &structs.ServiceIntentionEvent{Intention: intention},
	}, true
}
