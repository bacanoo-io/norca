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
