// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestFSM_CustomObjects(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	apply := func(msgType structs.MessageType, req any) any {
		t.Helper()
		// Sent with the ignore flag, as the RPC endpoints do
		buf, err := structs.Encode(msgType|structs.IgnoreUnknownTypeFlag, req)
		must.NoError(t, err)
		return fsm.Apply(makeLog(buf))
	}

	kind := &structs.ObjectKind{
		Group:      "db.example.com",
		Kind:       "PostgresCluster",
		Namespaced: true,
		Schema:     `{"type": "object"}`,
		Rules:      []*structs.ObjectKindRule{{Expression: "true", Message: "ok"}},
		Columns:    []*structs.ObjectKindColumn{{Name: "Version", Path: "spec.version"}},
		Scale:      &structs.ObjectKindScale{SpecPath: "spec.replicas", StatusPath: "status.replicas"},
	}
	must.Nil(t, apply(structs.ObjectKindUpsertRequestType, &structs.ObjectKindUpsertRequest{ObjectKind: kind}))

	obj := &structs.CustomObject{
		ID:         uuid.Generate(),
		Group:      kind.Group,
		Kind:       kind.Kind,
		Namespace:  "default",
		Name:       "billing",
		Labels:     map[string]string{"team": "finance"},
		ManagedBy:  "gitops",
		Owners:     []*structs.ObjectOwnerReference{{Kind: "Job", Namespace: "default", Name: "pg", ID: "x"}},
		Generation: 1,
		Spec: map[string]any{
			"version":  "16",
			"replicas": float64(3),
			"backup":   map[string]any{"schedule": "0 2 * * *"},
			"users":    []any{"a", "b"},
		},
		Status: map[string]any{"observedGeneration": float64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}
	must.Nil(t, apply(structs.CustomObjectUpsertRequestType, &structs.CustomObjectUpsertStateRequest{Object: obj}))

	// A stale check-and-set index is rejected by the FSM.
	resp := apply(structs.CustomObjectUpsertRequestType, &structs.CustomObjectUpsertStateRequest{Object: obj, ExpectedModifyIndex: 999})
	must.NotNil(t, resp)
	must.True(t, structs.IsObjectConflictError(resp.(error)))

	storedKind, err := fsm.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	must.NoError(t, err)
	must.NotNil(t, storedKind)
	stored, err := fsm.State().CustomObjectByName(nil, kind.Group, kind.Kind, "default", "billing")
	must.NoError(t, err)
	must.NotNil(t, stored)
	must.Eq(t, obj.Spec, stored.Spec)
	must.Eq(t, obj.Status, stored.Status)

	// Snapshot and restore keep kinds and objects, with nested values.
	fsm2 := testSnapshotRestore(t, fsm)
	kind2, err := fsm2.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	must.NoError(t, err)
	must.Eq(t, storedKind, kind2)
	obj2, err := fsm2.State().CustomObjectByName(nil, kind.Group, kind.Kind, "default", "billing")
	must.NoError(t, err)
	must.Eq(t, stored, obj2)
	idx, err := fsm2.State().Index("custom_objects")
	must.NoError(t, err)
	must.Eq(t, stored.ModifyIndex, idx)

	// The kind can't be deleted while it has objects.
	resp = apply(structs.ObjectKindDeleteRequestType, &structs.ObjectKindDeleteRequest{Group: kind.Group, Kind: kind.Kind})
	must.NotNil(t, resp)
	must.ErrorContains(t, resp.(error), "still has objects")

	must.Nil(t, apply(structs.CustomObjectDeleteRequestType, &structs.CustomObjectDeleteStateRequest{
		Group: kind.Group, Kind: kind.Kind, Namespace: "default", Name: "billing", ExpectedModifyIndex: stored.ModifyIndex,
	}))
	must.Nil(t, apply(structs.ObjectKindDeleteRequestType, &structs.ObjectKindDeleteRequest{Group: kind.Group, Kind: kind.Kind}))

	k, err := fsm.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	must.NoError(t, err)
	must.Nil(t, k)
}

func TestFSM_ObjectLifecycle(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	apply := func(msgType structs.MessageType, req any) any {
		t.Helper()
		buf, err := structs.Encode(msgType|structs.IgnoreUnknownTypeFlag, req)
		must.NoError(t, err)
		return fsm.Apply(makeLog(buf))
	}

	kind := &structs.ObjectKind{Group: "example.com", Kind: "Thing", Namespaced: true, Schema: `{"type": "object"}`}
	must.Nil(t, apply(structs.ObjectKindUpsertRequestType, &structs.ObjectKindUpsertRequest{ObjectKind: kind}))

	newObj := func(name string) *structs.CustomObject {
		return &structs.CustomObject{ID: uuid.Generate(), Group: kind.Group, Kind: kind.Kind, Namespace: "default",
			Name: name, Generation: 1, Spec: map[string]any{}}
	}

	// An object marked for deletion keeps its finalizers and deletion mode.
	marked := newObj("marked")
	marked.Finalizers = []string{"example.com/cleanup"}
	marked.DeletionTime = 123
	marked.DeletionPropagation = structs.ObjectPropagationForeground
	must.Nil(t, apply(structs.CustomObjectUpsertRequestType, &structs.CustomObjectUpsertStateRequest{Object: marked}))

	// The event log and a tombstone.
	logged := newObj("logged")
	must.Nil(t, apply(structs.CustomObjectUpsertRequestType, &structs.CustomObjectUpsertStateRequest{Object: logged}))
	must.Nil(t, apply(structs.CustomObjectEventsRequestType, &structs.CustomObjectEventsStateRequest{
		Group: kind.Group, Kind: kind.Kind, Namespace: "default", Name: "logged", ObjectID: logged.ID,
		Events: []*structs.ObjectEvent{{Reason: "Reconciled", Meta: map[string]string{"k": "v"}}}, Time: 42,
	}))
	gone := newObj("gone")
	must.Nil(t, apply(structs.CustomObjectUpsertRequestType, &structs.CustomObjectUpsertStateRequest{Object: gone}))
	must.Nil(t, apply(structs.CustomObjectDeleteRequestType, &structs.CustomObjectDeleteStateRequest{
		Group: kind.Group, Kind: kind.Kind, Namespace: "default", Name: "gone",
		Propagation: structs.ObjectPropagationBackground, DeleteTime: 7,
	}))

	// Events of a deleted object are dropped without failing the FSM.
	resp := apply(structs.CustomObjectEventsRequestType, &structs.CustomObjectEventsStateRequest{
		Group: kind.Group, Kind: kind.Kind, Namespace: "default", Name: "gone", ObjectID: gone.ID,
		Events: []*structs.ObjectEvent{{Reason: "Late"}},
	})
	must.NotNil(t, resp)

	fsm2 := testSnapshotRestore(t, fsm)
	m, err := fsm2.State().CustomObjectByName(nil, kind.Group, kind.Kind, "default", "marked")
	must.NoError(t, err)
	must.Eq(t, marked.Finalizers, m.Finalizers)
	must.Eq(t, int64(123), m.DeletionTime)
	must.Eq(t, structs.ObjectPropagationForeground, m.DeletionPropagation)

	log, err := fsm2.State().ObjectEventLog(nil, kind.Group, kind.Kind, "default", "logged")
	must.NoError(t, err)
	must.NotNil(t, log)
	must.Len(t, 1, log.Events)
	must.Eq(t, "v", log.Events[0].Meta["k"])
	must.Eq(t, int64(42), log.Events[0].FirstTime)

	iter, err := fsm2.State().ObjectTombstones(nil)
	must.NoError(t, err)
	raw := iter.Next()
	must.NotNil(t, raw)
	must.Eq(t, gone.ID, raw.(*structs.ObjectTombstone).ID)

	must.Nil(t, apply(structs.ObjectGCRequestType, &structs.ObjectGCRequest{DeleteTombstones: []string{gone.ID}}))
	iter, err = fsm.State().ObjectTombstones(nil)
	must.NoError(t, err)
	must.Nil(t, iter.Next())
}