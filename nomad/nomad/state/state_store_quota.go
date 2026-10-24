// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	TableQuotaSpecs = "quota_specs"
	TableQuotaUsage = "quota_usage"

	// indexQuota is the index of the namespaces table on the quota name.
	indexQuota = "quota"

	// indexNamespace is the index of the allocs table on the namespace.
	indexNamespace = "namespace"
)

func init() {
	RegisterSchemaFactories(quotaSpecTableSchema, quotaUsageTableSchema)
}

// quotaSpecTableSchema returns the MemDB schema of the quota specifications.
func quotaSpecTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableQuotaSpecs,
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

// quotaUsageTableSchema returns the MemDB schema of the quota usages.
func quotaUsageTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableQuotaUsage,
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

// QuotaSpecs returns an iterator over all quota specifications.
func (s *StateStore) QuotaSpecs(ws memdb.WatchSet, sort SortOption) (memdb.ResultIterator, error) {
	return s.quotaTableIter(ws, TableQuotaSpecs, indexID, sort)
}

// QuotaSpecsByNamePrefix returns an iterator over the quota specifications
// whose name matches the prefix.
func (s *StateStore) QuotaSpecsByNamePrefix(ws memdb.WatchSet, prefix string, sort SortOption) (memdb.ResultIterator, error) {
	return s.quotaTableIter(ws, TableQuotaSpecs, indexID+"_prefix", sort, prefix)
}

// QuotaUsages returns an iterator over all quota usages.
func (s *StateStore) QuotaUsages(ws memdb.WatchSet, sort SortOption) (memdb.ResultIterator, error) {
	return s.quotaTableIter(ws, TableQuotaUsage, indexID, sort)
}

// QuotaUsagesByNamePrefix returns an iterator over the quota usages whose
// name matches the prefix.
func (s *StateStore) QuotaUsagesByNamePrefix(ws memdb.WatchSet, prefix string, sort SortOption) (memdb.ResultIterator, error) {
	return s.quotaTableIter(ws, TableQuotaUsage, indexID+"_prefix", sort, prefix)
}

func (s *StateStore) quotaTableIter(ws memdb.WatchSet, table, index string, sort SortOption, args ...interface{}) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(table, index, args...)
	default:
		iter, err = txn.Get(table, index, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("%s lookup failed: %w", table, err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// QuotaSpecByName returns the quota specification with the given name or nil
// if it doesn't exist.
func (s *StateStore) QuotaSpecByName(ws memdb.WatchSet, name string) (*structs.QuotaSpec, error) {
	txn := s.db.ReadTxn()
	watchCh, existing, err := txn.FirstWatch(TableQuotaSpecs, indexID, name)
	if err != nil {
		return nil, fmt.Errorf("quota spec lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.QuotaSpec), nil
}

// QuotaUsageByName returns the usage of the quota with the given name or nil
// if it doesn't exist. The returned object must not be modified.
func (s *StateStore) QuotaUsageByName(ws memdb.WatchSet, name string) (*structs.QuotaUsage, error) {
	txn := s.db.ReadTxn()
	watchCh, existing, err := txn.FirstWatch(TableQuotaUsage, indexID, name)
	if err != nil {
		return nil, fmt.Errorf("quota usage lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.QuotaUsage), nil
}

// QuotaUsageWithVariables returns a copy of the usage with the size of the
// variables of the namespaces using the quota filled in (in MiB, rounded up).
// The variables size is tracked per namespace by the variables table and is
// aggregated at read time.
func (s *StateStore) QuotaUsageWithVariables(ws memdb.WatchSet, usage *structs.QuotaUsage) (*structs.QuotaUsage, error) {
	if usage == nil {
		return nil, nil
	}
	txn := s.db.ReadTxn()

	size, err := s.quotaVariablesSizeTxn(txn, ws, usage.Name, "", 0)
	if err != nil {
		return nil, err
	}
	mib := int((size + structs.BytesInMegabyte - 1) / structs.BytesInMegabyte)

	out := usage.Copy()
	for _, used := range out.Used {
		used.VariablesLimit = &mib
	}
	return out, nil
}

// NamespacesByQuota returns an iterator over the namespaces that use the
// given quota.
func (s *StateStore) NamespacesByQuota(ws memdb.WatchSet, quota string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableNamespaces, indexQuota, quota)
	if err != nil {
		return nil, fmt.Errorf("namespaces lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// QuotaForNamespace returns the quota specification attached to the
// namespace, or nil if the namespace doesn't exist or has no quota.
func (s *StateStore) QuotaForNamespace(ws memdb.WatchSet, namespace string) (*structs.QuotaSpec, error) {
	ns, err := s.NamespaceByName(ws, namespace)
	if err != nil {
		return nil, err
	}
	if ns == nil || ns.Quota == "" {
		return nil, nil
	}
	return s.QuotaSpecByName(ws, ns.Quota)
}

// QuotaRemainingForNamespace returns the remaining capacity of the quota
// attached to the namespace in the local region. It returns nil if the
// namespace has no quota or the quota doesn't limit the local region, which
// means there is no quota ceiling. This is the entry point the autoscaler
// uses to cap scale-ups at the quota ceiling.
func (s *StateStore) QuotaRemainingForNamespace(ws memdb.WatchSet, namespace string) (*structs.QuotaRemaining, error) {
	spec, err := s.QuotaForNamespace(ws, namespace)
	if err != nil || spec == nil {
		return nil, err
	}
	limit := spec.LimitForRegion(s.config.Region)
	if limit == nil {
		return nil, nil
	}

	usage, err := s.QuotaUsageByName(ws, spec.Name)
	if err != nil {
		return nil, err
	}

	var used structs.QuotaResources
	if entry := usage.RegionUsage(s.config.Region); entry != nil {
		used = entry.UsageResources()
	}

	rem := limit.Remaining(used)
	rem.Quota = spec.Name
	return rem, nil
}

// UpsertQuotaSpecs inserts or updates the given quota specifications and
// recomputes their usage.
func (s *StateStore) UpsertQuotaSpecs(msgType structs.MessageType, index uint64, specs []*structs.QuotaSpec) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, spec := range specs {
		if spec == nil {
			continue
		}
		if err := s.upsertQuotaSpecTxn(txn, index, spec); err != nil {
			return err
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableQuotaSpecs, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}

	return txn.Commit()
}

func (s *StateStore) upsertQuotaSpecTxn(txn WriteTxn, index uint64, spec *structs.QuotaSpec) error {
	// The hash should be set by the RPC endpoint, but compute it here as well
	// for defense in depth since usages are keyed by limit hashes.
	spec.Canonicalize()
	if len(spec.Hash) == 0 {
		spec.SetHash()
	}

	existing, err := txn.First(TableQuotaSpecs, indexID, spec.Name)
	if err != nil {
		return fmt.Errorf("quota spec lookup failed: %w", err)
	}
	if existing != nil {
		spec.CreateIndex = existing.(*structs.QuotaSpec).CreateIndex
	} else {
		spec.CreateIndex = index
	}
	spec.ModifyIndex = index

	if err := txn.Insert(TableQuotaSpecs, spec); err != nil {
		return fmt.Errorf("quota spec insert failed: %w", err)
	}

	return s.quotaRecomputeUsageTxn(txn, index, spec.Name)
}

// DeleteQuotaSpecs deletes the given quota specifications and their usage.
// A quota that is still referenced by a namespace can't be deleted.
func (s *StateStore) DeleteQuotaSpecs(msgType structs.MessageType, index uint64, names []string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, name := range names {
		existing, err := txn.First(TableQuotaSpecs, indexID, name)
		if err != nil {
			return fmt.Errorf("quota spec lookup failed: %w", err)
		}
		if existing == nil {
			return fmt.Errorf("quota %q not found", name)
		}

		nsRaw, err := txn.First(TableNamespaces, indexQuota, name)
		if err != nil {
			return fmt.Errorf("namespaces lookup failed: %w", err)
		}
		if nsRaw != nil {
			return fmt.Errorf("quota %q is in use by namespace %q", name, nsRaw.(*structs.Namespace).Name)
		}

		if err := txn.Delete(TableQuotaSpecs, existing); err != nil {
			return fmt.Errorf("quota spec delete failed: %w", err)
		}

		usage, err := txn.First(TableQuotaUsage, indexID, name)
		if err != nil {
			return fmt.Errorf("quota usage lookup failed: %w", err)
		}
		if usage != nil {
			if err := txn.Delete(TableQuotaUsage, usage); err != nil {
				return fmt.Errorf("quota usage delete failed: %w", err)
			}
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableQuotaSpecs, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableQuotaUsage, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}

	return txn.Commit()
}

// quotaSpecExists returns whether the quota exists.
func (s *StateStore) quotaSpecExists(txn ReadTxn, name string) (bool, error) {
	existing, err := txn.First(TableQuotaSpecs, indexID, name)
	return existing != nil, err
}

// quotaReconcile recomputes the usage of the quotas whose set of namespaces
// changed because a namespace switched from oldQuota to newQuota.
func (s *StateStore) quotaReconcile(index uint64, txn WriteTxn, newQuota, oldQuota string) error {
	if newQuota == oldQuota {
		return nil
	}
	for _, name := range []string{newQuota, oldQuota} {
		if name == "" {
			continue
		}
		if err := s.quotaRecomputeUsageTxn(txn, index, name); err != nil {
			return err
		}
	}
	return nil
}

// quotaRecomputeUsageTxn recomputes the usage of a quota from scratch by
// summing the non-terminal allocations of every namespace using it.
func (s *StateStore) quotaRecomputeUsageTxn(txn WriteTxn, index uint64, name string) error {
	specRaw, err := txn.First(TableQuotaSpecs, indexID, name)
	if err != nil {
		return fmt.Errorf("quota spec lookup failed: %w", err)
	}
	existingRaw, err := txn.First(TableQuotaUsage, indexID, name)
	if err != nil {
		return fmt.Errorf("quota usage lookup failed: %w", err)
	}

	if specRaw == nil {
		// Nothing to track
		return nil
	}
	spec := specRaw.(*structs.QuotaSpec)

	usage := structs.NewQuotaUsage(spec, s.config.Region)
	if existingRaw != nil {
		usage.CreateIndex = existingRaw.(*structs.QuotaUsage).CreateIndex
	} else {
		usage.CreateIndex = index
	}
	usage.ModifyIndex = index

	if len(usage.Used) != 0 {
		var used structs.QuotaResources
		nsIter, err := txn.Get(TableNamespaces, indexQuota, name)
		if err != nil {
			return fmt.Errorf("namespaces lookup failed: %w", err)
		}
		for raw := nsIter.Next(); raw != nil; raw = nsIter.Next() {
			ns := raw.(*structs.Namespace)
			allocIter, err := txn.Get(TableAllocs, indexNamespace, ns.Name)
			if err != nil {
				return fmt.Errorf("allocs lookup failed: %w", err)
			}
			for a := allocIter.Next(); a != nil; a = allocIter.Next() {
				used = used.Add(structs.AllocQuotaResources(a.(*structs.Allocation)))
			}
		}
		for _, entry := range usage.Used {
			entry.SetUsageResources(used)
		}
	}

	if err := txn.Insert(TableQuotaUsage, usage); err != nil {
		return fmt.Errorf("quota usage insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableQuotaUsage, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return nil
}

// quotaNameForNamespaceTxn returns the quota attached to the namespace.
func quotaNameForNamespaceTxn(txn ReadTxn, namespace string) (string, error) {
	raw, err := txn.First(TableNamespaces, indexID, namespace)
	if err != nil {
		return "", fmt.Errorf("namespace lookup failed: %w", err)
	}
	if raw == nil {
		return "", nil
	}
	return raw.(*structs.Namespace).Quota, nil
}

// updateEntWithAlloc is used to update the quota usage when an allocation is
// added, modified or deleted. It runs in the same transaction as the
// allocation write so the usage seen by the plan applier is always
// consistent with the allocations.
func (s *StateStore) updateEntWithAlloc(index uint64, new, existing *structs.Allocation, txn *txn) error {
	return s.updateQuotaWithAlloc(index, new, existing, txn)
}

func (s *StateStore) updateQuotaWithAlloc(index uint64, new, existing *structs.Allocation, txn WriteTxn) error {
	delta := structs.AllocQuotaResources(new).Sub(structs.AllocQuotaResources(existing))
	if delta.IsZero() {
		return nil
	}

	var namespace string
	switch {
	case new != nil:
		namespace = new.Namespace
	case existing != nil:
		namespace = existing.Namespace
	}

	quota, err := quotaNameForNamespaceTxn(txn, namespace)
	if err != nil || quota == "" {
		return err
	}

	raw, err := txn.First(TableQuotaUsage, indexID, quota)
	if err != nil {
		return fmt.Errorf("quota usage lookup failed: %w", err)
	}
	if raw == nil {
		return nil
	}
	usage := raw.(*structs.QuotaUsage)
	if len(usage.Used) == 0 {
		// The quota doesn't limit this region
		return nil
	}

	usage = usage.Copy()
	for _, entry := range usage.Used {
		entry.SetUsageResources(entry.UsageResources().Add(delta))
	}
	usage.ModifyIndex = index

	if err := txn.Insert(TableQuotaUsage, usage); err != nil {
		return fmt.Errorf("quota usage insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableQuotaUsage, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return nil
}

// quotaVariablesSizeTxn returns the total size in bytes of the variables of
// all namespaces using the quota. change is added to the size of the given
// namespace, for checks that run before the variables table is updated.
func (s *StateStore) quotaVariablesSizeTxn(txn ReadTxn, ws memdb.WatchSet, quota, namespace string, change int64) (int64, error) {
	iter, err := txn.Get(TableNamespaces, indexQuota, quota)
	if err != nil {
		return 0, fmt.Errorf("namespaces lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())

	var total int64
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		ns := raw.(*structs.Namespace)
		watchCh, vq, err := txn.FirstWatch(TableVariablesQuotas, indexID, ns.Name)
		if err != nil {
			return 0, fmt.Errorf("variable quota lookup failed: %w", err)
		}
		ws.Add(watchCh)
		if vq != nil {
			total += vq.(*structs.VariablesQuota).Size
		}
		if ns.Name == namespace {
			total += change
		}
	}
	return total, nil
}

// enforceVariablesQuota returns an error if growing the variables of the
// namespace by change bytes would exceed the variables limit of its quota in
// the local region. Shrinking is always allowed.
func (s *StateStore) enforceVariablesQuota(_ uint64, txn WriteTxn, namespace string, change int64) error {
	if change <= 0 {
		return nil
	}

	quota, err := quotaNameForNamespaceTxn(txn, namespace)
	if err != nil || quota == "" {
		return err
	}

	raw, err := txn.First(TableQuotaSpecs, indexID, quota)
	if err != nil {
		return fmt.Errorf("quota spec lookup failed: %w", err)
	}
	if raw == nil {
		return nil
	}
	limit := raw.(*structs.QuotaSpec).LimitForRegion(s.config.Region)
	if limit == nil || limit.VariablesLimit == nil || *limit.VariablesLimit == 0 {
		return nil
	}
	if *limit.VariablesLimit < 0 {
		return fmt.Errorf("quota %q exceeded: variables are not allowed", quota)
	}

	total, err := s.quotaVariablesSizeTxn(txn, nil, quota, namespace, change)
	if err != nil {
		return err
	}
	if allowed := int64(*limit.VariablesLimit) * structs.BytesInMegabyte; total > allowed {
		return fmt.Errorf("quota %q exceeded: variables would use %d bytes of %d allowed", quota, total, allowed)
	}
	return nil
}

// QuotaSpecRestore is used to restore a quota specification.
func (r *StateRestore) QuotaSpecRestore(spec *structs.QuotaSpec) error {
	if err := r.txn.Insert(TableQuotaSpecs, spec); err != nil {
		return fmt.Errorf("quota spec insert failed: %w", err)
	}
	return nil
}

// QuotaUsageRestore is used to restore a quota usage.
func (r *StateRestore) QuotaUsageRestore(usage *structs.QuotaUsage) error {
	if err := r.txn.Insert(TableQuotaUsage, usage); err != nil {
		return fmt.Errorf("quota usage insert failed: %w", err)
	}
	return nil
}
