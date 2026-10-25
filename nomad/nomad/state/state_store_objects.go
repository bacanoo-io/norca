// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// TableObjectKinds is the type registry of custom objects.
	TableObjectKinds = "object_kinds"

	// TableCustomObjects holds the custom objects.
	TableCustomObjects = "custom_objects"

	// indexObjectLabels indexes custom objects by label key and value.
	indexObjectLabels = "labels"
)

func init() {
	RegisterSchemaFactories(objectKindsTableSchema, customObjectsTableSchema,
		objectEventsTableSchema, objectTombstonesTableSchema)

	// Every write that produces a CustomObject event updates one of these
	// tables' index entries.
	topicIndexTables[structs.TopicCustomObject] = []string{TableObjectKinds, TableCustomObjects, TableObjectEvents}
}

// objectKindsTableSchema returns the MemDB schema of the object kinds table.
// Kinds are indexed by their "group/Kind" reference.
func objectKindsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableObjectKinds,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer:      objectKindIndexer{},
			},
		},
	}
}

// customObjectsTableSchema returns the MemDB schema of the custom objects
// table. The id index orders objects by kind, namespace and name, so the
// objects of a kind and of a kind in a namespace are prefix ranges.
func customObjectsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableCustomObjects,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer:      customObjectIndexer{},
			},
			indexNamespace: {
				Name:         indexNamespace,
				AllowMissing: true,
				Unique:       false,
				Indexer: &memdb.StringFieldIndex{
					Field: "Namespace",
				},
			},
			indexObjectLabels: {
				Name:         indexObjectLabels,
				AllowMissing: true,
				Unique:       false,
				Indexer: &memdb.StringMapFieldIndex{
					Field: "Labels",
				},
			},
		},
	}
}

// objectKindIndexer indexes object kinds by "group/Kind". Prefix lookups
// take a prefix of that reference.
type objectKindIndexer struct{}

func (objectKindIndexer) FromObject(raw any) (bool, []byte, error) {
	kind, ok := raw.(*structs.ObjectKind)
	if !ok {
		return false, nil, fmt.Errorf("unexpected type %T for object kind index", raw)
	}
	return true, []byte(kind.KindRef() + "\x00"), nil
}

func (objectKindIndexer) FromArgs(args ...any) ([]byte, error) {
	if len(args) != 1 {
		return nil, errors.New("must provide only a single argument")
	}
	ref, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("argument must be a string: %#v", args[0])
	}
	return []byte(ref + "\x00"), nil
}

func (objectKindIndexer) PrefixFromArgs(args ...any) ([]byte, error) {
	if len(args) != 1 {
		return nil, errors.New("must provide only a single argument")
	}
	prefix, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("argument must be a string: %#v", args[0])
	}
	return []byte(prefix), nil
}

// customObjectIndexer indexes custom objects by kind, namespace and name.
// FromArgs takes the "group/Kind" reference, the namespace and the name.
// PrefixFromArgs takes the reference, optionally the namespace and
// optionally a name prefix.
type customObjectIndexer struct{}

func (customObjectIndexer) FromObject(raw any) (bool, []byte, error) {
	obj, ok := raw.(*structs.CustomObject)
	if !ok {
		return false, nil, fmt.Errorf("unexpected type %T for custom object index", raw)
	}
	return true, []byte(obj.KindRef() + "\x00" + obj.Namespace + "\x00" + obj.Name + "\x00"), nil
}

func (customObjectIndexer) stringArgs(args []any) ([]string, error) {
	out := make([]string, len(args))
	for i, arg := range args {
		s, ok := arg.(string)
		if !ok {
			return nil, fmt.Errorf("argument %d must be a string: %#v", i, arg)
		}
		out[i] = s
	}
	return out, nil
}

func (c customObjectIndexer) FromArgs(args ...any) ([]byte, error) {
	if len(args) != 3 {
		return nil, errors.New("must provide the kind, the namespace and the name")
	}
	parts, err := c.stringArgs(args)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Join(parts, "\x00") + "\x00"), nil
}

func (c customObjectIndexer) PrefixFromArgs(args ...any) ([]byte, error) {
	if len(args) < 1 || len(args) > 3 {
		return nil, errors.New("must provide the kind and optionally the namespace and a name prefix")
	}
	parts, err := c.stringArgs(args)
	if err != nil {
		return nil, err
	}
	switch len(parts) {
	case 3:
		// The name is a prefix: no terminator after it.
		return []byte(parts[0] + "\x00" + parts[1] + "\x00" + parts[2]), nil
	default:
		return []byte(strings.Join(parts, "\x00") + "\x00"), nil
	}
}

// ObjectKinds returns an iterator over all object kinds, or over the kinds
// whose "group/Kind" reference starts with prefix.
func (s *StateStore) ObjectKinds(ws memdb.WatchSet, prefix string, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableObjectKinds, indexID+"_prefix", prefix)
	default:
		iter, err = txn.Get(TableObjectKinds, indexID+"_prefix", prefix)
	}
	if err != nil {
		return nil, fmt.Errorf("object kinds lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// ObjectKindByName returns the object kind or nil if it does not exist.
func (s *StateStore) ObjectKindByName(ws memdb.WatchSet, group, kind string) (*structs.ObjectKind, error) {
	txn := s.db.ReadTxn()
	return objectKindByNameTxn(txn, ws, group, kind)
}

func objectKindByNameTxn(txn ReadTxn, ws memdb.WatchSet, group, kind string) (*structs.ObjectKind, error) {
	watchCh, existing, err := txn.FirstWatch(TableObjectKinds, indexID, structs.ObjectKindRef(group, kind))
	if err != nil {
		return nil, fmt.Errorf("object kind lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.ObjectKind), nil
}

// UpsertObjectKind inserts or updates an object kind. A kind's scope
// (namespaced or not) can't change while objects of the kind exist.
func (s *StateStore) UpsertObjectKind(msgType structs.MessageType, index uint64, kind *structs.ObjectKind) error {
	return s.UpsertObjectKindCAS(msgType, index, kind, nil)
}

// UpsertObjectKindCAS is UpsertObjectKind with check-and-set: a non-nil cas
// is the expected modify index of the stored kind (0: it must not exist).
func (s *StateStore) UpsertObjectKindCAS(msgType structs.MessageType, index uint64, kind *structs.ObjectKind, cas *uint64) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	if kind == nil {
		return errors.New("missing object kind")
	}

	existing, err := objectKindByNameTxn(txn, nil, kind.Group, kind.Kind)
	if err != nil {
		return err
	}
	if cas != nil {
		switch {
		case existing == nil && *cas != 0:
			return structs.NewObjectConflictError("object kind %q does not exist", kind.KindRef())
		case existing != nil && existing.ModifyIndex != *cas:
			return structs.NewObjectConflictError("object kind %q has modify index %d, expected %d",
				kind.KindRef(), existing.ModifyIndex, *cas)
		}
	}
	if existing != nil {
		if existing.Namespaced != kind.Namespaced {
			hasObjects, err := kindHasObjectsTxn(txn, kind.KindRef())
			if err != nil {
				return err
			}
			if hasObjects {
				return structs.NewErrRPCCodedf(http.StatusConflict,
					"object kind %q has objects; its scope can't change", kind.KindRef())
			}
		}
		kind.CreateIndex = existing.CreateIndex
	} else {
		kind.CreateIndex = index
	}
	kind.ModifyIndex = index

	if err := txn.Insert(TableObjectKinds, kind); err != nil {
		return fmt.Errorf("object kind insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableObjectKinds, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteObjectKind removes an object kind. It fails if the kind does not
// exist or still has objects.
func (s *StateStore) DeleteObjectKind(msgType structs.MessageType, index uint64, group, kind string) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	existing, err := objectKindByNameTxn(txn, nil, group, kind)
	if err != nil {
		return err
	}
	if existing == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", structs.ObjectKindRef(group, kind))
	}
	hasObjects, err := kindHasObjectsTxn(txn, existing.KindRef())
	if err != nil {
		return err
	}
	if hasObjects {
		return structs.NewErrRPCCodedf(http.StatusConflict,
			"object kind %q still has objects; delete them first", existing.KindRef())
	}

	if err := txn.Delete(TableObjectKinds, existing); err != nil {
		return fmt.Errorf("object kind deletion failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableObjectKinds, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// kindHasObjectsTxn reports whether objects of the kind exist.
func kindHasObjectsTxn(txn ReadTxn, kindRef string) (bool, error) {
	iter, err := txn.Get(TableCustomObjects, indexID+"_prefix", kindRef)
	if err != nil {
		return false, fmt.Errorf("custom objects lookup failed: %w", err)
	}
	return iter.Next() != nil, nil
}

// CustomObjects returns an iterator over the objects of a kind, in the given
// namespace or, if namespace is "*", in all namespaces. Objects of a
// cluster-scoped kind have the namespace "".
func (s *StateStore) CustomObjects(ws memdb.WatchSet, group, kind, namespace string, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	args := []any{structs.ObjectKindRef(group, kind)}
	if namespace != structs.AllNamespacesSentinel {
		args = append(args, namespace)
	}

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableCustomObjects, indexID+"_prefix", args...)
	default:
		iter, err = txn.Get(TableCustomObjects, indexID+"_prefix", args...)
	}
	if err != nil {
		return nil, fmt.Errorf("custom objects lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// CustomObjectsByLabel returns an iterator over the objects of all kinds
// that have the label with the given value.
func (s *StateStore) CustomObjectsByLabel(ws memdb.WatchSet, key, value string, sort SortOption) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()

	var iter memdb.ResultIterator
	var err error
	switch sort {
	case SortReverse:
		iter, err = txn.GetReverse(TableCustomObjects, indexObjectLabels, key, value)
	default:
		iter, err = txn.Get(TableCustomObjects, indexObjectLabels, key, value)
	}
	if err != nil {
		return nil, fmt.Errorf("custom objects lookup failed: %w", err)
	}

	ws.Add(iter.WatchCh())
	return iter, nil
}

// CustomObjectsByNamespace returns an iterator over the objects of all
// kinds in a namespace.
func (s *StateStore) CustomObjectsByNamespace(ws memdb.WatchSet, namespace string) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableCustomObjects, indexNamespace, namespace)
	if err != nil {
		return nil, fmt.Errorf("custom objects lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// CustomObjectByName returns the object or nil if it does not exist.
func (s *StateStore) CustomObjectByName(ws memdb.WatchSet, group, kind, namespace, name string) (*structs.CustomObject, error) {
	txn := s.db.ReadTxn()
	return customObjectByNameTxn(txn, ws, structs.ObjectKindRef(group, kind), namespace, name)
}

func customObjectByNameTxn(txn ReadTxn, ws memdb.WatchSet, kindRef, namespace, name string) (*structs.CustomObject, error) {
	watchCh, existing, err := txn.FirstWatch(TableCustomObjects, indexID, kindRef, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("custom object lookup failed: %w", err)
	}
	ws.Add(watchCh)

	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.CustomObject), nil
}

// UpsertCustomObject writes an object computed by the server. The write is
// applied only if the stored object's modify index is expectedModifyIndex
// (0: the object must not exist); otherwise it fails with a conflict error,
// so concurrent writers never overwrite each other's changes. The kind must
// exist.
func (s *StateStore) UpsertCustomObject(msgType structs.MessageType, index uint64, obj *structs.CustomObject, expectedModifyIndex uint64) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	if obj == nil {
		return errors.New("missing custom object")
	}

	kind, err := objectKindByNameTxn(txn, nil, obj.Group, obj.Kind)
	if err != nil {
		return err
	}
	if kind == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", obj.KindRef())
	}

	existing, err := customObjectByNameTxn(txn, nil, obj.KindRef(), obj.Namespace, obj.Name)
	if err != nil {
		return err
	}
	switch {
	case existing == nil && expectedModifyIndex != 0:
		return structs.NewObjectConflictError("%s %q does not exist", obj.KindRef(), obj.Name)
	case existing != nil && existing.ModifyIndex != expectedModifyIndex:
		return structs.NewObjectConflictError("%s %q has modify index %d, expected %d",
			obj.KindRef(), obj.Name, existing.ModifyIndex, expectedModifyIndex)
	}

	if existing != nil {
		obj.CreateIndex = existing.CreateIndex
	} else {
		obj.CreateIndex = index
	}
	obj.ModifyIndex = index

	if err := txn.Insert(TableCustomObjects, obj); err != nil {
		return fmt.Errorf("custom object insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableCustomObjects, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// DeleteCustomObject removes an object. A non-zero expectedModifyIndex must
// match the stored object. It leaves no tombstone; see
// DeleteCustomObjectRequest.
func (s *StateStore) DeleteCustomObject(msgType structs.MessageType, index uint64, group, kind, namespace, name string, expectedModifyIndex uint64) error {
	return s.DeleteCustomObjectRequest(msgType, index, &structs.CustomObjectDeleteStateRequest{
		Group:               group,
		Kind:                kind,
		Namespace:           namespace,
		Name:                name,
		ExpectedModifyIndex: expectedModifyIndex,
	})
}

// DeleteCustomObjectRequest removes an object and its event log. A non-zero
// expected modify index must match the stored object. With the background
// and foreground deletion modes it records a tombstone for the garbage
// collector.
func (s *StateStore) DeleteCustomObjectRequest(msgType structs.MessageType, index uint64, req *structs.CustomObjectDeleteStateRequest) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	kindRef := structs.ObjectKindRef(req.Group, req.Kind)
	existing, err := customObjectByNameTxn(txn, nil, kindRef, req.Namespace, req.Name)
	if err != nil {
		return err
	}
	if existing == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kindRef, req.Name)
	}
	if req.ExpectedModifyIndex != 0 && existing.ModifyIndex != req.ExpectedModifyIndex {
		return structs.NewObjectConflictError("%s %q has modify index %d, expected %d",
			kindRef, req.Name, existing.ModifyIndex, req.ExpectedModifyIndex)
	}

	if err := txn.Delete(TableCustomObjects, existing); err != nil {
		return fmt.Errorf("custom object deletion failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableCustomObjects, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	if err := deleteObjectEventLogTxn(txn, index, kindRef, req.Namespace, req.Name); err != nil {
		return err
	}
	switch req.Propagation {
	case structs.ObjectPropagationBackground, structs.ObjectPropagationForeground:
		if err := insertObjectTombstoneTxn(txn, index, existing, req.Propagation, req.DeleteTime); err != nil {
			return err
		}
	}
	return txn.Commit()
}

// namespaceHasCustomObjectsTxn reports whether objects exist in the
// namespace.
func namespaceHasCustomObjectsTxn(txn ReadTxn, namespace string) (bool, error) {
	iter, err := txn.Get(TableCustomObjects, indexNamespace, namespace)
	if err != nil {
		return false, fmt.Errorf("custom objects lookup failed: %w", err)
	}
	return iter.Next() != nil, nil
}

// ObjectKindRestore is used to restore an object kind from a snapshot.
func (r *StateRestore) ObjectKindRestore(kind *structs.ObjectKind) error {
	if err := r.txn.Insert(TableObjectKinds, kind); err != nil {
		return fmt.Errorf("object kind insert failed: %w", err)
	}
	return nil
}

// CustomObjectRestore is used to restore a custom object from a snapshot.
func (r *StateRestore) CustomObjectRestore(obj *structs.CustomObject) error {
	if err := r.txn.Insert(TableCustomObjects, obj); err != nil {
		return fmt.Errorf("custom object insert failed: %w", err)
	}
	return nil
}

// objectEventFromChange returns the event stream event of a change in the
// object kinds or custom objects table.
func objectEventFromChange(change memdb.Change) (structs.Event, bool) {
	switch change.Table {
	case TableObjectKinds:
		obj := change.After
		eventType := structs.TypeObjectKindUpserted
		if change.Deleted() {
			obj = change.Before
			eventType = structs.TypeObjectKindDeleted
		}
		kind, ok := obj.(*structs.ObjectKind)
		if !ok {
			return structs.Event{}, false
		}
		return structs.Event{
			Topic:      structs.TopicCustomObject,
			Type:       eventType,
			Key:        kind.KindRef(),
			FilterKeys: []string{kind.KindRef()},
			Payload:    &structs.ObjectKindEvent{ObjectKind: kind},
		}, true

	case TableCustomObjects:
		obj := change.After
		eventType := structs.TypeCustomObjectUpserted
		if change.Deleted() {
			obj = change.Before
			eventType = structs.TypeCustomObjectDeleted
		}
		object, ok := obj.(*structs.CustomObject)
		if !ok {
			return structs.Event{}, false
		}
		if before, ok := change.Before.(*structs.CustomObject); ok && !change.Deleted() && onlyStatusChanged(before, object) {
			eventType = structs.TypeCustomObjectStatusUpdated
		}
		return structs.Event{
			Topic:      structs.TopicCustomObject,
			Type:       eventType,
			Key:        object.KindRef() + "/" + object.Name,
			FilterKeys: []string{object.KindRef()},
			Namespace:  object.Namespace,
			Payload:    &structs.CustomObjectEvent{Object: object},
		}, true

	case TableObjectEvents:
		return objectEventLogEventFromChange(change)
	}
	return structs.Event{}, false
}

// onlyStatusChanged reports whether a write changed nothing but the status.
func onlyStatusChanged(before, after *structs.CustomObject) bool {
	return before.Generation == after.Generation &&
		before.ManagedBy == after.ManagedBy &&
		before.DeletionTime == after.DeletionTime &&
		slices.Equal(before.Finalizers, after.Finalizers) &&
		mapsEqualString(before.Labels, after.Labels) &&
		structs.ObjectMapsEqual(before.Spec, after.Spec) &&
		!structs.ObjectMapsEqual(before.Status, after.Status)
}

func mapsEqualString(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
