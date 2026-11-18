// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
)

func TestStateStore_ObjectEventLog(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("example.com", "Thing", true)))
	obj := testCustomObject("example.com", "Thing", "default", "a")
	obj.ID = "id-a"
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 11, obj, 0))

	req := func(id string, events ...*structs.ObjectEvent) *structs.CustomObjectEventsStateRequest {
		return &structs.CustomObjectEventsStateRequest{
			Group: "example.com", Kind: "Thing", Namespace: "default", Name: "a",
			ObjectID: id, Events: events, Time: 1000, Limit: 3,
		}
	}

	// Events of another object with the same name are rejected.
	err := store.AppendObjectEvents(structs.MsgTypeTestSetup, 12, req("other", &structs.ObjectEvent{Reason: "Reconciled"}))
	must.ErrorContains(t, err, "not found")

	ws := memdb.NewWatchSet()
	log, err := store.ObjectEventLog(ws, "example.com", "Thing", "default", "a")
	must.NoError(t, err)
	must.Nil(t, log)

	// Repetitions of the newest event are counted.
	must.NoError(t, store.AppendObjectEvents(structs.MsgTypeTestSetup, 13, req("id-a",
		&structs.ObjectEvent{Reason: "Reconciled", Message: "ok"},
		&structs.ObjectEvent{Reason: "Reconciled", Message: "ok"})))
	must.True(t, watchFired(ws))
	log, err = store.ObjectEventLog(nil, "example.com", "Thing", "default", "a")
	must.NoError(t, err)
	must.Len(t, 1, log.Events)
	must.Eq(t, 2, log.Events[0].Count)
	must.Eq(t, structs.ObjectEventTypeNormal, log.Events[0].Type)
	must.Eq(t, uint64(13), log.Events[0].Index)

	// The log keeps the newest Limit events.
	for i := 0; i < 4; i++ {
		must.NoError(t, store.AppendObjectEvents(structs.MsgTypeTestSetup, uint64(20+i), req("id-a",
			&structs.ObjectEvent{Type: structs.ObjectEventTypeWarning, Reason: "Failed", Message: fmt.Sprintf("attempt %d", i)})))
	}
	log, err = store.ObjectEventLog(nil, "example.com", "Thing", "default", "a")
	must.NoError(t, err)
	must.Len(t, 3, log.Events)
	must.Eq(t, "attempt 1", log.Events[0].Message)
	must.Eq(t, "attempt 3", log.Events[2].Message)
	must.Eq(t, uint64(23), log.ModifyIndex)

	// Deleting the object leaves a tombstone in the background mode, and
	// the log stays with it.
	must.NoError(t, store.DeleteCustomObjectRequest(structs.MsgTypeTestSetup, 30, &structs.CustomObjectDeleteStateRequest{
		Group: "example.com", Kind: "Thing", Namespace: "default", Name: "a",
		Propagation: structs.ObjectPropagationBackground, DeleteTime: 5000,
	}))
	log, err = store.ObjectEventLog(nil, "example.com", "Thing", "default", "a")
	must.NoError(t, err)
	must.NotNil(t, log)
	must.Eq(t, "id-a", log.ObjectID)

	iter, err := store.ObjectTombstones(nil)
	must.NoError(t, err)
	raw := iter.Next()
	must.NotNil(t, raw)
	tomb := raw.(*structs.ObjectTombstone)
	must.Eq(t, "id-a", tomb.ID)
	must.Eq(t, int64(5000), tomb.DeleteTime)
	must.Eq(t, structs.ObjectPropagationBackground, tomb.Propagation)
	must.Nil(t, iter.Next())

	must.NoError(t, store.ApplyObjectGC(structs.MsgTypeTestSetup, 31, &structs.ObjectGCRequest{DeleteTombstones: []string{"id-a", "missing"}}))
	iter, err = store.ObjectTombstones(nil)
	must.NoError(t, err)
	must.Nil(t, iter.Next())
	log, err = store.ObjectEventLog(nil, "example.com", "Thing", "default", "a")
	must.NoError(t, err)
	must.Nil(t, log)
}

// TestStateStore_ObjectEventLogOfDeletedObject checks that the event log of
// a deleted object stays as long as its tombstone (review finding MUK-7):
// events recorded for the deleted object go into it, an object created
// again with the same name has a log of its own, and the log goes with the
// tombstone.
func TestStateStore_ObjectEventLogOfDeletedObject(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("example.com", "Thing", true)))

	write := func(index uint64, id string) {
		t.Helper()
		obj := testCustomObject("example.com", "Thing", "default", "a")
		obj.ID = id
		must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, index, obj, 0))
	}
	record := func(index uint64, id, reason string) error {
		return store.AppendObjectEvents(structs.MsgTypeTestSetup, index, &structs.CustomObjectEventsStateRequest{
			Group: "example.com", Kind: "Thing", Namespace: "default", Name: "a",
			ObjectID: id, Events: []*structs.ObjectEvent{{Reason: reason}}, Time: int64(index), Limit: 10,
		})
	}
	remove := func(index uint64, mode string) {
		t.Helper()
		must.NoError(t, store.DeleteCustomObjectRequest(structs.MsgTypeTestSetup, index, &structs.CustomObjectDeleteStateRequest{
			Group: "example.com", Kind: "Thing", Namespace: "default", Name: "a",
			Propagation: mode, DeleteTime: int64(index),
		}))
	}
	reasons := func() (string, []string) {
		t.Helper()
		log, err := store.ObjectEventLog(nil, "example.com", "Thing", "default", "a")
		must.NoError(t, err)
		if log == nil {
			return "", nil
		}
		var out []string
		for _, e := range log.Events {
			out = append(out, e.Reason)
		}
		return log.ObjectID, out
	}
	logCount := func() int {
		t.Helper()
		iter, err := store.ObjectEventLogs(nil)
		must.NoError(t, err)
		n := 0
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			n++
		}
		return n
	}

	write(11, "id-a")
	must.NoError(t, record(12, "id-a", "Reconciled"))
	ws := memdb.NewWatchSet()
	_, err := store.ObjectEventLog(ws, "example.com", "Thing", "default", "a")
	must.NoError(t, err)

	// The deleted object's log stays, and takes the events recorded for it
	// after the deletion.
	remove(20, structs.ObjectPropagationBackground)
	must.True(t, watchFired(ws))
	id, got := reasons()
	must.Eq(t, "id-a", id)
	must.Eq(t, []string{"Reconciled"}, got)
	must.NoError(t, record(21, "id-a", "Released"))
	_, got = reasons()
	must.Eq(t, []string{"Reconciled", "Released"}, got)
	must.ErrorContains(t, record(22, "other", "Released"), "not found")

	// An object of the same name created again doesn't show them, and has
	// a log of its own; the deleted object's log takes its events still.
	write(30, "id-b")
	id, got = reasons()
	must.Eq(t, "", id)
	must.Nil(t, got)
	must.NoError(t, record(31, "id-b", "Created"))
	must.NoError(t, record(32, "id-a", "Deleted"))
	id, got = reasons()
	must.Eq(t, "id-b", id)
	must.Eq(t, []string{"Created"}, got)
	must.Eq(t, 2, logCount())

	// The log goes with the tombstone.
	must.NoError(t, store.ApplyObjectGC(structs.MsgTypeTestSetup, 40, &structs.ObjectGCRequest{DeleteTombstones: []string{"id-a"}}))
	must.Eq(t, 1, logCount())
	must.ErrorContains(t, record(41, "id-a", "Released"), "not found")
	id, _ = reasons()
	must.Eq(t, "id-b", id)

	// A deletion that leaves no tombstone takes the log at once.
	remove(50, "")
	must.Eq(t, 0, logCount())
	id, _ = reasons()
	must.Eq(t, "", id)
}

func TestStateStore_ObjectTombstonePropagation(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("example.com", "Thing", true)))

	for i, mode := range []string{"", structs.ObjectPropagationOrphan, structs.ObjectPropagationForeground} {
		obj := testCustomObject("example.com", "Thing", "default", fmt.Sprintf("o%d", i))
		obj.ID = fmt.Sprintf("id-%d", i)
		must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, uint64(20+i), obj, 0))
		must.NoError(t, store.DeleteCustomObjectRequest(structs.MsgTypeTestSetup, uint64(30+i), &structs.CustomObjectDeleteStateRequest{
			Group: "example.com", Kind: "Thing", Namespace: "default", Name: obj.Name, Propagation: mode,
		}))
	}

	// The orphan and the foreground deletion left a tombstone, with their
	// mode: the event log of the object stays with it. The empty mode (the
	// servers' own removals without a mode) leaves none.
	iter, err := store.ObjectTombstones(nil)
	must.NoError(t, err)
	var ids, modes []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		ids = append(ids, raw.(*structs.ObjectTombstone).ID)
		modes = append(modes, raw.(*structs.ObjectTombstone).Propagation)
	}
	must.Eq(t, []string{"id-1", "id-2"}, ids)
	must.Eq(t, []string{structs.ObjectPropagationOrphan, structs.ObjectPropagationForeground}, modes)
}

func TestStateStore_ObjectEventLogStreamEvent(t *testing.T) {
	ci.Parallel(t)
	store := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer store.StopEventBroker()
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("example.com", "Thing", true)))
	obj := testCustomObject("example.com", "Thing", "default", "a")
	obj.ID = "id-a"
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 11, obj, 0))

	msgType := structs.CustomObjectEventsRequestType
	must.NoError(t, store.AppendObjectEvents(msgType, 12, &structs.CustomObjectEventsStateRequest{
		Group: "example.com", Kind: "Thing", Namespace: "default", Name: "a", ObjectID: "id-a",
		Events: []*structs.ObjectEvent{{Reason: "Reconciled"}}, Time: 1,
	}))

	events := WaitForEvents(t, store, 12, 1, time.Second)
	must.Len(t, 1, events)
	e := events[0]
	must.Eq(t, structs.TopicCustomObject, e.Topic)
	must.Eq(t, structs.TypeCustomObjectEventRecorded, e.Type)
	must.Eq(t, "example.com/Thing/a", e.Key)
	must.Eq(t, []string{"example.com/Thing"}, e.FilterKeys)
	payload := e.Payload.(*structs.CustomObjectEventsEvent)
	must.Len(t, 1, payload.Events)
	must.Eq(t, "Reconciled", payload.Events[0].Reason)
}
