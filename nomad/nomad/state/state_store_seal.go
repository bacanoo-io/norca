// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"
	"slices"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TableSealConfig holds the keyring seal configuration of the region, a
// single structs.SealConfig. It contains no key material (seal types, key
// IDs, Shamir parameters, key checks) and produces no events.
const TableSealConfig = "seal_config"

func init() {
	RegisterSchemaFactories(sealConfigTableSchema)
}

func sealConfigTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableSealConfig,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: true,
				Unique:       true,
				Indexer:      singletonRecord,
			},
		},
	}
}

// SealConfig returns the seal configuration of the region, or nil if none
// was written (the region uses the aead seal). The returned value is the
// state store's own copy and must not be modified.
func (s *StateStore) SealConfig(ws memdb.WatchSet) (*structs.SealConfig, error) {
	txn := s.db.ReadTxn()
	watchCh, raw, err := txn.FirstWatch(TableSealConfig, indexID)
	if err != nil {
		return nil, fmt.Errorf("seal config lookup failed: %w", err)
	}
	ws.Add(watchCh)
	if raw == nil {
		return nil, nil
	}
	return raw.(*structs.SealConfig), nil
}

// UpsertSealConfig writes the seal configuration of the region.
func (s *StateStore) UpsertSealConfig(msgType structs.MessageType, index uint64, cfg *structs.SealConfig) error {
	if cfg == nil {
		return fmt.Errorf("missing seal config")
	}
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	cfg = cfg.Copy()
	existing, err := txn.First(TableSealConfig, indexID)
	if err != nil {
		return fmt.Errorf("seal config lookup failed: %w", err)
	}
	if existing != nil {
		cfg.CreateIndex = existing.(*structs.SealConfig).CreateIndex
	} else {
		cfg.CreateIndex = index
	}
	cfg.ModifyIndex = index
	if err := txn.Insert(TableSealConfig, cfg); err != nil {
		return fmt.Errorf("seal config insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSealConfig, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// SealRewrapDone records that a server has finished the rewrap with the
// given ID. A report for another rewrap than the one in progress is ignored.
func (s *StateStore) SealRewrapDone(msgType structs.MessageType, index uint64, rewrapID, serverID string) error {
	return s.SealRewrapReport(msgType, index, rewrapID, serverID, false)
}

// SealRewrapReport records that a server has finished the rewrap with the
// given ID, or with sealed set, that it is sealed and can't take part. A
// server that finished is never moved back to sealed.
func (s *StateStore) SealRewrapReport(msgType structs.MessageType, index uint64, rewrapID, serverID string, sealed bool) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	raw, err := txn.First(TableSealConfig, indexID)
	if err != nil {
		return fmt.Errorf("seal config lookup failed: %w", err)
	}
	if raw == nil {
		return nil
	}
	cfg := raw.(*structs.SealConfig)
	if cfg.Rewrap == nil || cfg.Rewrap.ID != rewrapID || serverID == "" ||
		slices.Contains(cfg.Rewrap.Done, serverID) ||
		(sealed && slices.Contains(cfg.Rewrap.Sealed, serverID)) {
		return nil
	}
	cfg = cfg.Copy()
	if sealed {
		cfg.Rewrap.Sealed = append(cfg.Rewrap.Sealed, serverID)
		slices.Sort(cfg.Rewrap.Sealed)
	} else {
		cfg.Rewrap.Done = append(cfg.Rewrap.Done, serverID)
		slices.Sort(cfg.Rewrap.Done)
		cfg.Rewrap.Sealed = slices.DeleteFunc(cfg.Rewrap.Sealed, func(id string) bool { return id == serverID })
	}
	cfg.ModifyIndex = index
	if err := txn.Insert(TableSealConfig, cfg); err != nil {
		return fmt.Errorf("seal config insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableSealConfig, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// SealConfigRestore restores the seal configuration from a snapshot.
func (r *StateRestore) SealConfigRestore(cfg *structs.SealConfig) error {
	if err := r.txn.Insert(TableSealConfig, cfg); err != nil {
		return fmt.Errorf("seal config insert failed: %w", err)
	}
	return nil
}
