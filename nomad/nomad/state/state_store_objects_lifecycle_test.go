// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
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

	// Deleting the object deletes its log and leaves a tombstone in the
	// background mode.
	must.NoError(t, store.DeleteCustomObjectRequest(structs.MsgTypeTestSetup, 30, &structs.CustomObjectDeleteStateRequest{
		Group: "example.com", Kind: "Thing", Namespace: "default", Name: "a",
		Propagation: structs.ObjectPropagationBackground, DeleteTime: 5000,
	}))
	log, err = store.ObjectEventLog(nil, "example.com", "Thing", "default", "a")
	must.NoError(t, err)
	must.Nil(t, log)

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

	// Only the foreground deletion left a tombstone: the empty mode (old
	// servers) and orphan don't.
	iter, err := store.ObjectTombstones(nil)
	must.NoError(t, err)
	var ids []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		ids = append(ids, raw.(*structs.ObjectTombstone).ID)
	}
	must.Eq(t, []string{"id-2"}, ids)
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
