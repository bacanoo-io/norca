// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestEventsFromChanges_Variables(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	secret := []byte("super-secret-ciphertext")
	sv := mock.VariableEncrypted()
	sv.Path = "crd/example/Widget/a"
	sv.Data = secret

	// Create.
	resp := s.VarSet(1000, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: varPtr(sv.Copy())})
	must.NoError(t, resp.Error)

	events := WaitForEvents(t, s, 1000, 1, time.Second)
	must.Len(t, 1, events)
	e := events[0]
	must.Eq(t, structs.TopicVariables, e.Topic)
	must.Eq(t, structs.TypeVariableUpserted, e.Type)
	must.Eq(t, sv.Path, e.Key)
	must.Eq(t, sv.Namespace, e.Namespace)
	payload := e.Payload.(*structs.VariableEvent)
	must.Eq(t, sv.Path, payload.Variable.Path)
	must.Eq(t, 1000, payload.Variable.CreateIndex)
	must.Eq(t, 1000, payload.Variable.ModifyIndex)
	assertNoVariableData(t, e, secret, sv.KeyID)

	// Check-and-set update.
	update := varPtr(sv.Copy())
	update.ModifyIndex = 1000
	update.Data = []byte("another-ciphertext")
	resp = s.VarSetCAS(1001, &structs.VarApplyStateRequest{Op: structs.VarOpCAS, Var: update})
	must.NoError(t, resp.Error)
	must.False(t, resp.IsConflict())

	events = WaitForEvents(t, s, 1001, 1, time.Second)
	must.Len(t, 1, events)
	payload = events[0].Payload.(*structs.VariableEvent)
	must.Eq(t, structs.TypeVariableUpserted, events[0].Type)
	must.Eq(t, 1000, payload.Variable.CreateIndex)
	must.Eq(t, 1001, payload.Variable.ModifyIndex)
	assertNoVariableData(t, events[0], update.Data, sv.KeyID)

	// A no-op write produces no event.
	resp = s.VarSet(1002, &structs.VarApplyStateRequest{Op: structs.VarOpSet, Var: varPtr(update.Copy())})
	must.NoError(t, resp.Error)

	// Delete.
	resp = s.VarDelete(1003, &structs.VarApplyStateRequest{Op: structs.VarOpDelete, Var: varPtr(sv.Copy())})
	must.NoError(t, resp.Error)

	events = WaitForEvents(t, s, 1002, 1, time.Second)
	must.Len(t, 1, events)
	must.Eq(t, 1003, events[0].Index)
	must.Eq(t, structs.TypeVariableDeleted, events[0].Type)
	must.Eq(t, sv.Path, events[0].Key)
	payload = events[0].Payload.(*structs.VariableEvent)
	must.Eq(t, 1001, payload.Variable.ModifyIndex)
	assertNoVariableData(t, events[0], update.Data, sv.KeyID)
}

func varPtr(v structs.VariableEncrypted) *structs.VariableEncrypted { return &v }

// assertNoVariableData checks that the serialized event does not contain the
// variable's data or key ID.
func assertNoVariableData(t *testing.T, e structs.Event, data []byte, keyID string) {
	t.Helper()
	buf, err := json.Marshal(e)
	must.NoError(t, err)
	encoded, err := json.Marshal(data)
	must.NoError(t, err)
	must.StrNotContains(t, string(buf), string(encoded[1:len(encoded)-1]))
	must.StrNotContains(t, string(buf), "KeyID")
	must.StrNotContains(t, string(buf), keyID)
	must.StrNotContains(t, string(buf), `"Data"`)
}

func TestStateRestore_SetsEventBaseIndexes(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	restore, err := s.Restore()
	must.NoError(t, err)
	must.NoError(t, restore.IndexRestore(&IndexEntry{Key: TableVariables, Value: 40}))
	must.NoError(t, restore.IndexRestore(&IndexEntry{Key: "jobs", Value: 100}))
	must.NoError(t, restore.Commit())

	broker, err := s.EventBroker()
	must.NoError(t, err)

	subscribe := func(topic structs.Topic, index uint64) error {
		sub, err := broker.Subscribe(&stream.SubscribeRequest{
			Topics:              map[structs.Topic][]string{topic: {"*"}},
			Namespace:           "default",
			Index:               index,
			StartExactlyAtIndex: true,
		})
		if err == nil {
			sub.Unsubscribe()
		}
		return err
	}

	// Variables did not change after index 40 in the restored state.
	must.True(t, errors.Is(subscribe(structs.TopicVariables, 40), stream.ErrIndexNotAvailable))
	must.NoError(t, subscribe(structs.TopicVariables, 41))

	// Other topics fall back to the latest restored index.
	must.True(t, errors.Is(subscribe(structs.TopicJob, 100), stream.ErrIndexNotAvailable))
	must.NoError(t, subscribe(structs.TopicJob, 101))
}
