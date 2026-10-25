// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"errors"
	"fmt"
	"net/http"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// TableObjectEvents holds the event logs of custom objects, one entry
	// per object.
	TableObjectEvents = "object_events"

	// TableObjectTombstones holds the tombstones of deleted objects whose
	// dependents the garbage collector has to delete.
	TableObjectTombstones = "object_tombstones"
)

// objectEventsTableSchema returns the MemDB schema of the object event logs.
// They are indexed like the objects, by kind, namespace and name.
func objectEventsTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableObjectEvents,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer:      objectEventLogIndexer{},
			},
		},
	}
}

// objectTombstonesTableSchema returns the MemDB schema of the object
// tombstones, indexed by the ID of the deleted object.
func objectTombstonesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableObjectTombstones,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field: "ID",
				},
			},
		},
	}
}

// objectEventLogIndexer indexes event logs by kind, namespace and name, with
// the arguments of customObjectIndexer.
type objectEventLogIndexer struct {
	customObjectIndexer
}

func (objectEventLogIndexer) FromObject(raw any) (bool, []byte, error) {
	log, ok := raw.(*structs.ObjectEventLog)
	if !ok {
		return false, nil, fmt.Errorf("unexpected type %T for object event log index", raw)
	}
	return true, []byte(log.KindRef() + "\x00" + log.Namespace + "\x00" + log.Name + "\x00"), nil
}

// ObjectEventLog returns the event log of an object, or nil.
func (s *StateStore) ObjectEventLog(ws memdb.WatchSet, group, kind, namespace, name string) (*structs.ObjectEventLog, error) {
	txn := s.db.ReadTxn()
	return objectEventLogTxn(txn, ws, structs.ObjectKindRef(group, kind), namespace, name)
}

func objectEventLogTxn(txn ReadTxn, ws memdb.WatchSet, kindRef, namespace, name string) (*structs.ObjectEventLog, error) {
	watchCh, existing, err := txn.FirstWatch(TableObjectEvents, indexID, kindRef, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("object event log lookup failed: %w", err)
	}
	ws.Add(watchCh)
	if existing == nil {
		return nil, nil
	}
	return existing.(*structs.ObjectEventLog), nil
}

// ObjectEventLogs returns an iterator over all object event logs.
func (s *StateStore) ObjectEventLogs(ws memdb.WatchSet) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableObjectEvents, indexID)
	if err != nil {
		return nil, fmt.Errorf("object event logs lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// AppendObjectEvents adds events to the log of an object. The object must
// exist and have the ID of the request, so events of a deleted object are
// not attached to a new object with the same name.
func (s *StateStore) AppendObjectEvents(msgType structs.MessageType, index uint64, req *structs.CustomObjectEventsStateRequest) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	kindRef := structs.ObjectKindRef(req.Group, req.Kind)
	obj, err := customObjectByNameTxn(txn, nil, kindRef, req.Namespace, req.Name)
	if err != nil {
		return err
	}
	if obj == nil || (req.ObjectID != "" && obj.ID != req.ObjectID) {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kindRef, req.Name)
	}

	existing, err := objectEventLogTxn(txn, nil, kindRef, req.Namespace, req.Name)
	if err != nil {
		return err
	}
	var log *structs.ObjectEventLog
	if existing != nil && existing.ObjectID == obj.ID {
		log = existing.Copy()
	} else {
		log = &structs.ObjectEventLog{
			Group:       req.Group,
			Kind:        req.Kind,
			Namespace:   req.Namespace,
			Name:        req.Name,
			ObjectID:    obj.ID,
			CreateIndex: index,
		}
	}
	log.Append(req.Events, index, req.Time, req.Limit)
	log.ModifyIndex = index

	if err := txn.Insert(TableObjectEvents, log); err != nil {
		return fmt.Errorf("object event log insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableObjectEvents, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// deleteObjectEventLogTxn removes the event log of an object, if any.
func deleteObjectEventLogTxn(txn *txn, index uint64, kindRef, namespace, name string) error {
	existing, err := objectEventLogTxn(txn, nil, kindRef, namespace, name)
	if err != nil || existing == nil {
		return err
	}
	if err := txn.Delete(TableObjectEvents, existing); err != nil {
		return fmt.Errorf("object event log deletion failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableObjectEvents, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return nil
}

// insertObjectTombstoneTxn records the deletion of an object.
func insertObjectTombstoneTxn(txn *txn, index uint64, obj *structs.CustomObject, propagation string, deleteTime int64) error {
	if obj.ID == "" {
		return nil
	}
	tombstone := &structs.ObjectTombstone{
		ID:          obj.ID,
		Group:       obj.Group,
		Kind:        obj.Kind,
		Namespace:   obj.Namespace,
		Name:        obj.Name,
		Propagation: propagation,
		DeleteTime:  deleteTime,
		CreateIndex: index,
	}
	if err := txn.Insert(TableObjectTombstones, tombstone); err != nil {
		return fmt.Errorf("object tombstone insert failed: %w", err)
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableObjectTombstones, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return nil
}

// ObjectTombstones returns an iterator over the tombstones of deleted
// objects.
func (s *StateStore) ObjectTombstones(ws memdb.WatchSet) (memdb.ResultIterator, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableObjectTombstones, indexID)
	if err != nil {
		return nil, fmt.Errorf("object tombstones lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	return iter, nil
}

// ApplyObjectGC applies the bookkeeping of the object garbage collector: it
// removes processed tombstones. Missing tombstones are ignored.
func (s *StateStore) ApplyObjectGC(msgType structs.MessageType, index uint64, req *structs.ObjectGCRequest) error {
	if req == nil {
		return errors.New("missing object gc request")
	}
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, id := range req.DeleteTombstones {
		existing, err := txn.First(TableObjectTombstones, indexID, id)
		if err != nil {
			return fmt.Errorf("object tombstone lookup failed: %w", err)
		}
		if existing == nil {
			continue
		}
		if err := txn.Delete(TableObjectTombstones, existing); err != nil {
			return fmt.Errorf("object tombstone deletion failed: %w", err)
		}
	}
	if err := txn.Insert(tableIndex, &IndexEntry{TableObjectTombstones, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// ObjectEventLogRestore is used to restore an object event log from a
// snapshot.
func (r *StateRestore) ObjectEventLogRestore(log *structs.ObjectEventLog) error {
	if err := r.txn.Insert(TableObjectEvents, log); err != nil {
		return fmt.Errorf("object event log insert failed: %w", err)
	}
	return nil
}

// ObjectTombstoneRestore is used to restore an object tombstone from a
// snapshot.
func (r *StateRestore) ObjectTombstoneRestore(tombstone *structs.ObjectTombstone) error {
	if err := r.txn.Insert(TableObjectTombstones, tombstone); err != nil {
		return fmt.Errorf("object tombstone insert failed: %w", err)
	}
	return nil
}

// objectEventLogEventFromChange returns the CustomObjectEventRecorded event
// of an event log write. It carries the entries written at the index of the
// write; removed logs produce no event.
func objectEventLogEventFromChange(change memdb.Change) (structs.Event, bool) {
	if change.Deleted() {
		return structs.Event{}, false
	}
	log, ok := change.After.(*structs.ObjectEventLog)
	if !ok {
		return structs.Event{}, false
	}
	var events []*structs.ObjectEvent
	for _, e := range log.Events {
		if e.Index == log.ModifyIndex {
			events = append(events, e)
		}
	}
	return structs.Event{
		Topic:      structs.TopicCustomObject,
		Type:       structs.TypeCustomObjectEventRecorded,
		Key:        log.KindRef() + "/" + log.Name,
		FilterKeys: []string{log.KindRef()},
		Namespace:  log.Namespace,
		Payload: &structs.CustomObjectEventsEvent{
			Group:     log.Group,
			Kind:      log.Kind,
			Namespace: log.Namespace,
			Name:      log.Name,
			Events:    events,
		},
	}, true
}
