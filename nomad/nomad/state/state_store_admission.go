// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TableAdmissionPolicies is the table of the CEL admission policies.
const TableAdmissionPolicies = "admission_policies"

func init() {
	RegisterSchemaFactories(admissionPoliciesTableSchema)
}

// admissionPoliciesTableSchema returns the MemDB schema for the admission
// policies table.
func admissionPoliciesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableAdmissionPolicies,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field: "Name",
				},
			},
		},
	}
}

// AdmissionPolicies returns an iterator over all admission policies.
func (s *StateStore) AdmissionPolicies(ws memdb.WatchSet, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableAdmissionPolicies, indexID)
	default:
		iter, err = txn.Get(TableAdmissionPolicies, indexID)
	}
	if err != nil {
		return nil, fmt.Errorf("admission policies lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// AdmissionPoliciesByNamePrefix returns an iterator over all admission
// policies whose name matches the given prefix.
func (s *StateStore) AdmissionPoliciesByNamePrefix(ws memdb.WatchSet, prefix string, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableAdmissionPolicies, indexID+"_prefix", prefix)
	default:
		iter, err = txn.Get(TableAdmissionPolicies, indexID+"_prefix", prefix)
	}
	if err != nil {
		return nil, fmt.Errorf("admission policies prefix lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// AdmissionPolicyByName returns the admission policy with the given name or
// nil if there is none.
func (s *StateStore) AdmissionPolicyByName(ws memdb.WatchSet, name string) (*structs.AdmissionPolicy, error) {
	txn := s.db.ReadTxn()

	watchCh, existing, err := txn.FirstWatch(TableAdmissionPolicies, indexID, name)
	if err != nil {
		return nil, fmt.Errorf("admission policy lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.AdmissionPolicy), nil
}

// UpsertAdmissionPolicies inserts or updates the given admission policies.
func (s *StateStore) UpsertAdmissionPolicies(msgType structs.MessageType, index uint64, policies []*structs.AdmissionPolicy) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, policy := range policies {
		if policy == nil {
			continue
		}

		existing, err := txn.First(TableAdmissionPolicies, indexID, policy.Name)
		if err != nil {
			return fmt.Errorf("admission policy lookup failed: %w", err)
		}
		if existing != nil {
			policy.CreateIndex = existing.(*structs.AdmissionPolicy).CreateIndex
		} else {
			policy.CreateIndex = index
		}
		policy.ModifyIndex = index

		if err := txn.Insert(TableAdmissionPolicies, policy); err != nil {
			return fmt.Errorf("admission policy insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableAdmissionPolicies, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteAdmissionPolicies removes the admission policies with the given
// names. It fails if one of them does not exist.
func (s *StateStore) DeleteAdmissionPolicies(msgType structs.MessageType, index uint64, names []string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, name := range names {
		existing, err := txn.First(TableAdmissionPolicies, indexID, name)
		if err != nil {
			return fmt.Errorf("admission policy lookup failed: %w", err)
		}
		if existing == nil {
			return fmt.Errorf("admission policy %q not found", name)
		}
		if err := txn.Delete(TableAdmissionPolicies, existing); err != nil {
			return fmt.Errorf("admission policy deletion failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableAdmissionPolicies, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// AdmissionPolicyRestore is used to restore an admission policy from a
// snapshot.
func (r *StateRestore) AdmissionPolicyRestore(policy *structs.AdmissionPolicy) error {
	if err := r.txn.Insert(TableAdmissionPolicies, policy); err != nil {
		return fmt.Errorf("admission policy insert failed: %w", err)
	}
	return nil
}

// admissionPolicyEventFromChange returns the event stream event of a change
// in the admission policies table.
func admissionPolicyEventFromChange(change memdb.Change) (structs.Event, bool) {
	obj := change.After
	if change.Deleted() {
		obj = change.Before
	}
	policy, ok := obj.(*structs.AdmissionPolicy)
	if !ok {
		return structs.Event{}, false
	}
	return structs.Event{
		Topic: structs.TopicAdmissionPolicy,
		Key:   policy.Name,
		Payload: &structs.AdmissionPolicyEvent{
			AdmissionPolicy: policy,
		},
	}, true
}
