// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	memdb "github.com/hashicorp/go-memdb"
)

const (
	// TableObjectEvents holds the event logs of custom objects, one entry
	// per object: of a live object, and of a deleted object while its
	// tombstone is kept.
	TableObjectEvents = "object_events"

	// TableObjectTombstones holds the tombstones of deleted objects: for the
	// garbage collector, which deletes their dependents (not for an orphan
	// deletion), and for their event logs, which are kept as long.
	TableObjectTombstones = "object_tombstones"
)

// objectEventsTableSchema returns the MemDB schema of the object event logs.
// They are indexed by kind, namespace, name and the ID of the object, so the
// log of a deleted object and the log of an object created again with its
// name are kept apart.
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

// objectEventLogIndexer indexes event logs by kind, namespace, name and
// object ID. FromArgs takes the "group/Kind" reference, the namespace, the
// name and the object ID; PrefixFromArgs takes the first three, and finds
// the logs of that name only.
type objectEventLogIndexer struct {
	customObjectIndexer
}

func (objectEventLogIndexer) FromObject(raw any) (bool, []byte, error) {
	log, ok := raw.(*structs.ObjectEventLog)
	if !ok {
		return false, nil, fmt.Errorf("unexpected type %T for object event log index", raw)
	}
	return true, []byte(log.KindRef() + "\x00" + log.Namespace + "\x00" + log.Name + "\x00" + log.ObjectID + "\x00"), nil
}

func (c objectEventLogIndexer) FromArgs(args ...any) ([]byte, error) {
	if len(args) != 4 {
		return nil, errors.New("must provide the kind, the namespace, the name and the object ID")
	}
	parts, err := c.stringArgs(args)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Join(parts, "\x00") + "\x00"), nil
}

func (c objectEventLogIndexer) PrefixFromArgs(args ...any) ([]byte, error) {
	if len(args) != 3 {
		return nil, errors.New("must provide the kind, the namespace and the name")
	}
	parts, err := c.stringArgs(args)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Join(parts, "\x00") + "\x00"), nil
}

// ObjectEventLog returns the event log of the object of a name, or nil: the
// log of the object if it exists, otherwise the log of the object of that
// name deleted last, while its tombstone is kept. The log of a deleted
// object is never returned for an object created again with its name.
func (s *StateStore) ObjectEventLog(ws memdb.WatchSet, group, kind, namespace, name string) (*structs.ObjectEventLog, error) {
	txn := s.db.ReadTxn()
	return objectEventLogTxn(txn, ws, structs.ObjectKindRef(group, kind), namespace, name)
}

func objectEventLogTxn(txn ReadTxn, ws memdb.WatchSet, kindRef, namespace, name string) (*structs.ObjectEventLog, error) {
	obj, err := customObjectByNameTxn(txn, ws, kindRef, namespace, name)
	if err != nil {
		return nil, err
	}
	if obj != nil {
		return objectEventLogByIDTxn(txn, ws, kindRef, namespace, name, obj.ID)
	}
	iter, err := txn.Get(TableObjectEvents, indexID+"_prefix", kindRef, namespace, name)
	if err != nil {
		return nil, fmt.Errorf("object event log lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())
	var newest *structs.ObjectEventLog
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		log := raw.(*structs.ObjectEventLog)
		if newest == nil || log.CreateIndex > newest.CreateIndex {
			newest = log
		}
	}
	return newest, nil
}

// objectEventLogByIDTxn returns the event log of the object with the ID, or
// nil.
func objectEventLogByIDTxn(txn ReadTxn, ws memdb.WatchSet, kindRef, namespace, name, objectID string) (*structs.ObjectEventLog, error) {
	watchCh, existing, err := txn.FirstWatch(TableObjectEvents, indexID, kindRef, namespace, name, objectID)
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
// not attached to a new object with the same name. A deleted object whose
// tombstone is kept takes events too, in the log kept with the tombstone,
// when the request names its ID.
func (s *StateStore) AppendObjectEvents(msgType structs.MessageType, index uint64, req *structs.CustomObjectEventsStateRequest) error {
	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	kindRef := structs.ObjectKindRef(req.Group, req.Kind)
	obj, err := customObjectByNameTxn(txn, nil, kindRef, req.Namespace, req.Name)
	if err != nil {
		return err
	}
	var objectID string
	switch {
	case obj != nil && (req.ObjectID == "" || obj.ID == req.ObjectID):
		objectID = obj.ID
	case req.ObjectID != "":
		raw, err := txn.First(TableObjectTombstones, indexID, req.ObjectID)
		if err != nil {
			return fmt.Errorf("object tombstone lookup failed: %w", err)
		}
		t, ok := raw.(*structs.ObjectTombstone)
		if !ok || t.KindRef() != kindRef || t.Namespace != req.Namespace || t.Name != req.Name {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kindRef, req.Name)
		}
		objectID = t.ID
	default:
		return structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kindRef, req.Name)
	}

	existing, err := objectEventLogByIDTxn(txn, nil, kindRef, req.Namespace, req.Name, objectID)
	if err != nil {
		return err
	}
	var log *structs.ObjectEventLog
	if existing != nil {
		log = existing.Copy()
	} else {
		log = &structs.ObjectEventLog{
			Group:       req.Group,
			Kind:        req.Kind,
			Namespace:   req.Namespace,
			Name:        req.Name,
			ObjectID:    objectID,
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

// deleteObjectEventLogTxn removes the event log of the object with the ID,
// if any.
func deleteObjectEventLogTxn(txn *txn, index uint64, kindRef, namespace, name, objectID string) error {
	existing, err := objectEventLogByIDTxn(txn, nil, kindRef, namespace, name, objectID)
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
func insertObjectTombstoneTxn(txn *txn, index uint64, obj *structs.CustomObject, propagation string, deleteTime int64, approved []string) error {
	if obj.ID == "" {
		return nil
	}
	tombstone := &structs.ObjectTombstone{
		ID:                 obj.ID,
		Group:              obj.Group,
		Kind:               obj.Kind,
		Namespace:          obj.Namespace,
		Name:               obj.Name,
		Propagation:        propagation,
		DeleteTime:         deleteTime,
		ApprovedDependents: slices.Clone(approved),
		CreateIndex:        index,
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
// removes processed tombstones, and with them the event logs of their
// objects. Missing tombstones are ignored.
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
		t := existing.(*structs.ObjectTombstone)
		if err := deleteObjectEventLogTxn(txn, index, t.KindRef(), t.Namespace, t.Name, t.ID); err != nil {
			return err
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
