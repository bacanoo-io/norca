// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// TableNetworkPolicies is the table of the namespace network policies.
	TableNetworkPolicies = "network_policies"

	// TableAdminNetworkPolicies is the table of the cluster level admin
	// network policies.
	TableAdminNetworkPolicies = "admin_network_policies"
)

func init() {
	RegisterSchemaFactories(networkPoliciesTableSchema, adminNetworkPoliciesTableSchema)
}

// networkPoliciesTableSchema returns the MemDB schema of the network policies
// table. The ID index is the namespace and the name of the policy.
func networkPoliciesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableNetworkPolicies,
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
			indexNamespace: {
				Name:         indexNamespace,
				AllowMissing: false,
				Unique:       false,
				Indexer:      &memdb.StringFieldIndex{Field: "Namespace"},
			},
		},
	}
}

// adminNetworkPoliciesTableSchema returns the MemDB schema of the admin
// network policies table.
func adminNetworkPoliciesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableAdminNetworkPolicies,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer:      &memdb.StringFieldIndex{Field: "Name"},
			},
		},
	}
}

// NetworkPolicies returns an iterator over the network policies of all
// namespaces, ordered by namespace and name.
func (s *StateStore) NetworkPolicies(ws memdb.WatchSet, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableNetworkPolicies, indexID)
	default:
		iter, err = txn.Get(TableNetworkPolicies, indexID)
	}
	if err != nil {
		return nil, fmt.Errorf("network policies lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// NetworkPoliciesByNamespace returns an iterator over the network policies
// of a namespace.
func (s *StateStore) NetworkPoliciesByNamespace(ws memdb.WatchSet, namespace string, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	return networkPoliciesByNamespaceTxn(txn, ws, namespace, sort)
}

func networkPoliciesByNamespaceTxn(tx *txn, ws memdb.WatchSet, namespace string, sort SortOption) (memdb.ResultIterator, error) {
	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = tx.GetReverse(TableNetworkPolicies, indexNamespace, namespace)
	default:
		iter, err = tx.Get(TableNetworkPolicies, indexNamespace, namespace)
	}
	if err != nil {
		return nil, fmt.Errorf("network policies lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// NetworkPolicyByName returns the network policy of the namespace with the
// given name or nil.
func (s *StateStore) NetworkPolicyByName(ws memdb.WatchSet, namespace, name string) (*structs.NetworkPolicy, error) {
	txn := s.db.ReadTxn()

	watchCh, existing, err := txn.FirstWatch(TableNetworkPolicies, indexID, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("network policy lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.NetworkPolicy), nil
}

// UpsertNetworkPolicies inserts or updates network policies.
func (s *StateStore) UpsertNetworkPolicies(msgType structs.MessageType, index uint64, policies []*structs.NetworkPolicy) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, policy := range policies {
		if policy == nil {
			continue
		}
		existing, err := txn.First(TableNetworkPolicies, indexID, policy.Namespace, policy.Name)
		if err != nil {
			return fmt.Errorf("network policy lookup failed: %w", err)
		}
		if existing != nil {
			policy.CreateIndex = existing.(*structs.NetworkPolicy).CreateIndex
		} else {
			policy.CreateIndex = index
		}
		policy.ModifyIndex = index

		if err := txn.Insert(TableNetworkPolicies, policy); err != nil {
			return fmt.Errorf("network policy insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableNetworkPolicies, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteNetworkPolicies removes the network policies with the given names
// from the namespace. It fails if one of them does not exist.
func (s *StateStore) DeleteNetworkPolicies(msgType structs.MessageType, index uint64, namespace string, names []string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, name := range names {
		existing, err := txn.First(TableNetworkPolicies, indexID, namespace, name)
		if err != nil {
			return fmt.Errorf("network policy lookup failed: %w", err)
		}
		if existing == nil {
			return fmt.Errorf("network policy %q not found in namespace %q", name, namespace)
		}
		if err := txn.Delete(TableNetworkPolicies, existing); err != nil {
			return fmt.Errorf("network policy deletion failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableNetworkPolicies, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// AdminNetworkPolicies returns an iterator over the admin network policies,
// ordered by name.
func (s *StateStore) AdminNetworkPolicies(ws memdb.WatchSet, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableAdminNetworkPolicies, indexID)
	default:
		iter, err = txn.Get(TableAdminNetworkPolicies, indexID)
	}
	if err != nil {
		return nil, fmt.Errorf("admin network policies lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// AdminNetworkPolicyByName returns the admin network policy with the given
// name or nil.
func (s *StateStore) AdminNetworkPolicyByName(ws memdb.WatchSet, name string) (*structs.AdminNetworkPolicy, error) {
	txn := s.db.ReadTxn()

	watchCh, existing, err := txn.FirstWatch(TableAdminNetworkPolicies, indexID, name)
	if err != nil {
		return nil, fmt.Errorf("admin network policy lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.AdminNetworkPolicy), nil
}

// UpsertAdminNetworkPolicies inserts or updates admin network policies.
func (s *StateStore) UpsertAdminNetworkPolicies(msgType structs.MessageType, index uint64, policies []*structs.AdminNetworkPolicy) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, policy := range policies {
		if policy == nil {
			continue
		}
		existing, err := txn.First(TableAdminNetworkPolicies, indexID, policy.Name)
		if err != nil {
			return fmt.Errorf("admin network policy lookup failed: %w", err)
		}
		if existing != nil {
			policy.CreateIndex = existing.(*structs.AdminNetworkPolicy).CreateIndex
		} else {
			policy.CreateIndex = index
		}
		policy.ModifyIndex = index

		if err := txn.Insert(TableAdminNetworkPolicies, policy); err != nil {
			return fmt.Errorf("admin network policy insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableAdminNetworkPolicies, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteAdminNetworkPolicies removes the admin network policies with the
// given names. It fails if one of them does not exist.
func (s *StateStore) DeleteAdminNetworkPolicies(msgType structs.MessageType, index uint64, names []string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, name := range names {
		existing, err := txn.First(TableAdminNetworkPolicies, indexID, name)
		if err != nil {
			return fmt.Errorf("admin network policy lookup failed: %w", err)
		}
		if existing == nil {
			return fmt.Errorf("admin network policy %q not found", name)
		}
		if err := txn.Delete(TableAdminNetworkPolicies, existing); err != nil {
			return fmt.Errorf("admin network policy deletion failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableAdminNetworkPolicies, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// NetworkPoliciesForGroup returns the namespace policies and the admin
// policies (in evaluation order) that select the allocations of a task
// group in the namespace with the given merged meta.
func (s *StateStore) NetworkPoliciesForGroup(ws memdb.WatchSet, namespace string, meta map[string]string) ([]*structs.NetworkPolicy, []*structs.AdminNetworkPolicy, error) {
	txn := s.db.ReadTxn()

	iter, err := networkPoliciesByNamespaceTxn(txn, ws, namespace, SortDefault)
	if err != nil {
		return nil, nil, err
	}
	var policies []*structs.NetworkPolicy
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		policy := raw.(*structs.NetworkPolicy)
		if policy.Selects(namespace, meta) {
			policies = append(policies, policy)
		}
	}

	adminIter, err := txn.Get(TableAdminNetworkPolicies, indexID)
	if err != nil {
		return nil, nil, fmt.Errorf("admin network policies lookup failed: %w", err)
	}
	ws.Add(adminIter.WatchCh())
	var admin []*structs.AdminNetworkPolicy
	for raw := adminIter.Next(); raw != nil; raw = adminIter.Next() {
		policy := raw.(*structs.AdminNetworkPolicy)
		if policy.Selects(namespace, meta) {
			admin = append(admin, policy)
		}
	}
	structs.SortAdminNetworkPolicies(admin)
	return policies, admin, nil
}

// NetworkPolicyRestore is used to restore a network policy from a snapshot.
func (r *StateRestore) NetworkPolicyRestore(policy *structs.NetworkPolicy) error {
	if err := r.txn.Insert(TableNetworkPolicies, policy); err != nil {
		return fmt.Errorf("network policy insert failed: %w", err)
	}
	return nil
}

// AdminNetworkPolicyRestore is used to restore an admin network policy from
// a snapshot.
func (r *StateRestore) AdminNetworkPolicyRestore(policy *structs.AdminNetworkPolicy) error {
	if err := r.txn.Insert(TableAdminNetworkPolicies, policy); err != nil {
		return fmt.Errorf("admin network policy insert failed: %w", err)
	}
	return nil
}

// networkPolicyEventFromChange returns the event stream event of a change in
// one of the network policy tables. The event type is set by the caller from
// the message type.
func networkPolicyEventFromChange(change memdb.Change) (structs.Event, bool) {
	obj := change.After
	if change.Deleted() {
		obj = change.Before
	}
	switch policy := obj.(type) {
	case *structs.NetworkPolicy:
		return structs.Event{
			Topic:     structs.TopicNetworkPolicy,
			Key:       policy.Name,
			Namespace: policy.Namespace,
			Payload:   &structs.NetworkPolicyEvent{NetworkPolicy: policy},
		}, true
	case *structs.AdminNetworkPolicy:
		return structs.Event{
			Topic:   structs.TopicNetworkPolicy,
			Key:     policy.Name,
			Payload: &structs.AdminNetworkPolicyEvent{AdminNetworkPolicy: policy},
		}, true
	}
	return structs.Event{}, false
}
