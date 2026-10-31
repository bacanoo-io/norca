// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// The tables of the dynamic secrets of the built-in engine: the engines of
// the namespaces and the leases of the credentials they created. Neither
// contains a credential: an engine refers to its admin credential by secret
// path, a lease holds the backend's reference of its credential.
const (
	TableSecretEngines = "secret_engines"
	TableSecretLeases  = "secret_leases"

	// indexEngine indexes the leases by namespace and engine.
	indexEngine = "engine"
)

func init() {
	RegisterSchemaFactories(secretEnginesTableSchema, secretLeasesTableSchema)
}

// secretEnginesTableSchema returns the MemDB schema of the engines, indexed
// by namespace and name.
func secretEnginesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableSecretEngines,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.CompoundIndex{
					Indexes: []memdb.Indexer{
						&memdb.StringFieldIndex{Field: "Namespace"},
						&memdb.StringFieldIndex{Field: "Name"},
					},
				},
			},
		},
	}
}

// secretLeasesTableSchema returns the MemDB schema of the leases, indexed by
// ID and by namespace and engine.
func secretLeasesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableSecretLeases,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer:      &memdb.StringFieldIndex{Field: "ID"},
			},
			indexEngine: {
				Name:         indexEngine,
				AllowMissing: false,
				Unique:       false,
				Indexer: &memdb.CompoundIndex{
					Indexes: []memdb.Indexer{
						&memdb.StringFieldIndex{Field: "Namespace"},
						&memdb.StringFieldIndex{Field: "Engine"},
					},
				},
			},
		},
	}
}

// SecretEngines returns an iterator over the engines of all namespaces.
func (s *StateStore) SecretEngines(ws memdb.WatchSet) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableSecretEngines, indexID)
	if err != nil {
		return nil, fmt.Errorf("secret engines lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// SecretEnginesByNamespacePrefix returns an iterator over the engines of a
// namespace whose name starts with prefix.
func (s *StateStore) SecretEnginesByNamespacePrefix(ws memdb.WatchSet, namespace, prefix string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableSecretEngines, indexID+"_prefix", namespace, prefix)
	if err != nil {
		return nil, fmt.Errorf("secret engines lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// SecretEngineByName returns the engine of the namespace, or nil. The
// returned engine is the state store's own copy and must not be modified.
func (s *StateStore) SecretEngineByName(ws memdb.WatchSet, namespace, name string) (*structs.SecretEngine, error) {
	txn := s.db.ReadTxn()
	watchCh, existing, err := txn.FirstWatch(TableSecretEngines, indexID, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("secret engine lookup failed: %w", err)
	}
	ws.Add(watchCh)
	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.SecretEngine), nil
}

// ApplySecretEngine writes an engine. It fails with
// structs.ErrSecretConflict, without changing anything, if the engine's
// ModifyIndex is not req.ExpectedIndex (0: the engine must not exist).
func (s *StateStore) ApplySecretEngine(msgType structs.MessageType, index uint64, req *structs.SecretEngineApplyRequest) error {
	engine := req.Engine
	if engine == nil {
		return fmt.Errorf("secret engine apply without an engine")
	}
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	raw, err := txn.First(TableSecretEngines, indexID, engine.Namespace, engine.Name)
	if err != nil {
		return fmt.Errorf("secret engine lookup failed: %w", err)
	}
	var current uint64
	if raw != nil {
		existing := raw.(*structs.SecretEngine)
		current = existing.ModifyIndex
		engine.CreateIndex = existing.CreateIndex
		engine.CreateTime = existing.CreateTime
	} else {
		engine.CreateIndex = index
	}
	if current != req.ExpectedIndex {
		return structs.ErrSecretConflict
	}
	engine.ModifyIndex = index
	if err := txn.Insert(TableSecretEngines, engine); err != nil {
		return fmt.Errorf("secret engine insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSecretEngines, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// PurgeSecretEngine removes an engine and all of its leases. It fails with
// structs.ErrSecretConflict if the engine's ModifyIndex is not
// req.ExpectedIndex.
func (s *StateStore) PurgeSecretEngine(msgType structs.MessageType, index uint64, req *structs.SecretEnginePurgeRequest) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	raw, err := txn.First(TableSecretEngines, indexID, req.Namespace, req.Name)
	if err != nil {
		return fmt.Errorf("secret engine lookup failed: %w", err)
	}
	if raw == nil {
		return structs.ErrSecretEngineNotFound
	}
	if raw.(*structs.SecretEngine).ModifyIndex != req.ExpectedIndex {
		return structs.ErrSecretConflict
	}
	if err := txn.Delete(TableSecretEngines, raw); err != nil {
		return fmt.Errorf("secret engine deletion failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSecretEngines, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}

	iter, err := txn.Get(TableSecretLeases, indexEngine, req.Namespace, req.Name)
	if err != nil {
		return fmt.Errorf("secret leases lookup failed: %w", err)
	}
	var leases []any
	for l := iter.Next(); l != nil; l = iter.Next() {
		leases = append(leases, l)
	}
	for _, l := range leases {
		if err := txn.Delete(TableSecretLeases, l); err != nil {
			return fmt.Errorf("secret lease deletion failed: %w", err)
		}
	}
	if len(leases) > 0 {
		if err := txn.Insert(tableIndex, &IndexEntry{TableSecretLeases, index}); err != nil {
			return fmt.Errorf("index update failed: %w", err)
		}
	}
	return txn.Commit()
}

// SecretLeases returns an iterator over the leases of all namespaces.
func (s *StateStore) SecretLeases(ws memdb.WatchSet) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableSecretLeases, indexID)
	if err != nil {
		return nil, fmt.Errorf("secret leases lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// SecretLeasesByEngine returns an iterator over the leases of a namespace,
// of one engine if engine is not empty.
func (s *StateStore) SecretLeasesByEngine(ws memdb.WatchSet, namespace, engine string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	var iter memdb.ResultIterator
	var err error
	if engine == "" {
		iter, err = txn.Get(TableSecretLeases, indexEngine+"_prefix", namespace, "")
	} else {
		iter, err = txn.Get(TableSecretLeases, indexEngine, namespace, engine)
	}
	if err != nil {
		return nil, fmt.Errorf("secret leases lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// SecretLeaseByID returns the lease, or nil. The returned lease is the
// state store's own copy and must not be modified.
func (s *StateStore) SecretLeaseByID(ws memdb.WatchSet, id string) (*structs.SecretLease, error) {
	txn := s.db.ReadTxn()
	watchCh, existing, err := txn.FirstWatch(TableSecretLeases, indexID, id)
	if err != nil {
		return nil, fmt.Errorf("secret lease lookup failed: %w", err)
	}
	ws.Add(watchCh)
	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.SecretLease), nil
}

// ApplySecretLeases creates, renews and revokes leases in one transaction.
// A new lease requires its engine. Revocation is final: an update that
// would make a revoked lease live again (a renewal computed before the
// revocation committed) fails with structs.ErrSecretLeaseRevoked, without
// changing anything.
func (s *StateStore) ApplySecretLeases(msgType structs.MessageType, index uint64, req *structs.SecretLeaseApplyRequest) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, lease := range req.Leases {
		if lease == nil || lease.ID == "" {
			return fmt.Errorf("secret lease without an ID")
		}
		raw, err := txn.First(TableSecretLeases, indexID, lease.ID)
		if err != nil {
			return fmt.Errorf("secret lease lookup failed: %w", err)
		}
		if raw != nil {
			existing := raw.(*structs.SecretLease)
			if existing.Revoked && !lease.Revoked {
				return structs.ErrSecretLeaseRevoked
			}
			if existing.Revoked {
				// Keep the first revocation.
				lease.RevokeTime, lease.RevokeReason = existing.RevokeTime, existing.RevokeReason
			}
			lease.CreateIndex = existing.CreateIndex
		} else {
			engine, err := txn.First(TableSecretEngines, indexID, lease.Namespace, lease.Engine)
			if err != nil {
				return fmt.Errorf("secret engine lookup failed: %w", err)
			}
			if engine == nil {
				return structs.ErrSecretEngineNotFound
			}
			lease.CreateIndex = index
		}
		lease.ModifyIndex = index
		if err := txn.Insert(TableSecretLeases, lease); err != nil {
			return fmt.Errorf("secret lease insert failed: %w", err)
		}
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSecretLeases, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// PurgeSecretLeases removes leases; unknown IDs are ignored.
func (s *StateStore) PurgeSecretLeases(msgType structs.MessageType, index uint64, req *structs.SecretLeasePurgeRequest) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, id := range req.IDs {
		raw, err := txn.First(TableSecretLeases, indexID, id)
		if err != nil {
			return fmt.Errorf("secret lease lookup failed: %w", err)
		}
		if raw == nil {
			continue
		}
		if err := txn.Delete(TableSecretLeases, raw); err != nil {
			return fmt.Errorf("secret lease deletion failed: %w", err)
		}
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSecretLeases, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// SecretEngineRestore is used to restore an engine from a snapshot.
func (r *StateRestore) SecretEngineRestore(engine *structs.SecretEngine) error {
	if err := r.txn.Insert(TableSecretEngines, engine); err != nil {
		return fmt.Errorf("secret engine insert failed: %w", err)
	}
	return nil
}

// SecretLeaseRestore is used to restore a lease from a snapshot.
func (r *StateRestore) SecretLeaseRestore(lease *structs.SecretLease) error {
	if err := r.txn.Insert(TableSecretLeases, lease); err != nil {
		return fmt.Errorf("secret lease insert failed: %w", err)
	}
	return nil
}

// secretEngineEventFromChange builds the Secret topic event of a change of
// the engines or leases tables. Keys are the engine's name and, for leases,
// the lease ID; neither carries a credential.
func secretEngineEventFromChange(change memdb.Change) (structs.Event, bool) {
	switch change.Table {
	case TableSecretEngines:
		if change.Deleted() {
			before, ok := change.Before.(*structs.SecretEngine)
			if !ok {
				return structs.Event{}, false
			}
			return structs.Event{
				Topic:     structs.TopicSecret,
				Type:      structs.TypeSecretEngineDeleted,
				Key:       before.Name,
				Namespace: before.Namespace,
				Payload:   &structs.SecretEngineEvent{Engine: before.Copy()},
			}, true
		}
		after, ok := change.After.(*structs.SecretEngine)
		if !ok {
			return structs.Event{}, false
		}
		return structs.Event{
			Topic:     structs.TopicSecret,
			Type:      structs.TypeSecretEngineUpserted,
			Key:       after.Name,
			Namespace: after.Namespace,
			Payload:   &structs.SecretEngineEvent{Engine: after.Copy()},
		}, true

	case TableSecretLeases:
		if change.Deleted() {
			before, ok := change.Before.(*structs.SecretLease)
			if !ok {
				return structs.Event{}, false
			}
			return structs.Event{
				Topic:      structs.TopicSecret,
				Type:       structs.TypeSecretLeaseDeleted,
				Key:        before.ID,
				FilterKeys: []string{before.Engine},
				Namespace:  before.Namespace,
				Payload:    &structs.SecretLeaseEvent{Lease: before.Copy()},
			}, true
		}
		after, ok := change.After.(*structs.SecretLease)
		if !ok {
			return structs.Event{}, false
		}
		typ := structs.TypeSecretLeaseRenewed
		before, _ := change.Before.(*structs.SecretLease)
		switch {
		case before == nil:
			typ = structs.TypeSecretLeaseCreated
		case after.Revoked && !before.Revoked:
			typ = structs.TypeSecretLeaseRevoked
		case after.Revoked:
			return structs.Event{}, false
		}
		return structs.Event{
			Topic:      structs.TopicSecret,
			Type:       typ,
			Key:        after.ID,
			FilterKeys: []string{after.Engine},
			Namespace:  after.Namespace,
			Payload:    &structs.SecretLeaseEvent{Lease: after.Copy()},
		}, true
	}
	return structs.Event{}, false
}
