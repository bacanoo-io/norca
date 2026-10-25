// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"
	"time"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testObjectKind(group, kind string, namespaced bool) *structs.ObjectKind {
	return &structs.ObjectKind{
		Group:      group,
		Kind:       kind,
		Namespaced: namespaced,
		Schema:     `{"type": "object"}`,
	}
}

func testCustomObject(group, kind, ns, name string) *structs.CustomObject {
	return &structs.CustomObject{
		Group:      group,
		Kind:       kind,
		Namespace:  ns,
		Name:       name,
		Generation: 1,
		Spec:       map[string]any{"replicas": float64(1)},
	}
}

func countObjects(t *testing.T, iter memdb.ResultIterator, err error) []string {
	t.Helper()
	must.NoError(t, err)
	var names []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		switch v := raw.(type) {
		case *structs.CustomObject:
			names = append(names, v.Namespace+"/"+v.Name)
		case *structs.ObjectKind:
			names = append(names, v.KindRef())
		}
	}
	return names
}

func TestStateStore_ObjectKinds(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	ws := memdb.NewWatchSet()
	k, err := store.ObjectKindByName(ws, "db.example.com", "Postgres")
	must.NoError(t, err)
	must.Nil(t, k)

	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("db.example.com", "Postgres", true)))
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 11, testObjectKind("db.example.com", "PostgresBackup", true)))
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 12, testObjectKind("lb.example.com", "LoadBalancer", false)))
	must.True(t, watchFired(ws))

	k, err = store.ObjectKindByName(nil, "db.example.com", "Postgres")
	must.NoError(t, err)
	must.Eq(t, uint64(10), k.CreateIndex)

	iter, err := store.ObjectKinds(nil, "", SortDefault)
	must.Eq(t, []string{"db.example.com/Postgres", "db.example.com/PostgresBackup", "lb.example.com/LoadBalancer"}, countObjects(t, iter, err))
	iter, err = store.ObjectKinds(nil, "db.", SortReverse)
	must.Eq(t, []string{"db.example.com/PostgresBackup", "db.example.com/Postgres"}, countObjects(t, iter, err))

	// Update keeps the create index.
	kind := testObjectKind("db.example.com", "Postgres", true)
	kind.Description = "updated"
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 13, kind))
	k, err = store.ObjectKindByName(nil, "db.example.com", "Postgres")
	must.NoError(t, err)
	must.Eq(t, uint64(10), k.CreateIndex)
	must.Eq(t, uint64(13), k.ModifyIndex)

	// A kind with objects can't be deleted or change its scope.
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 14, testCustomObject("db.example.com", "Postgres", "default", "a"), 0))
	must.ErrorContains(t, store.DeleteObjectKind(structs.MsgTypeTestSetup, 15, "db.example.com", "Postgres"), "still has objects")
	must.ErrorContains(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 15, testObjectKind("db.example.com", "Postgres", false)), "scope")

	// A kind whose name is a prefix of another kind's has its own objects.
	must.NoError(t, store.DeleteObjectKind(structs.MsgTypeTestSetup, 16, "db.example.com", "PostgresBackup"))
	must.ErrorContains(t, store.DeleteObjectKind(structs.MsgTypeTestSetup, 17, "db.example.com", "PostgresBackup"), "not found")

	idx, err := store.Index(TableObjectKinds)
	must.NoError(t, err)
	must.Eq(t, uint64(16), idx)
}

func TestStateStore_CustomObjects(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	// The kind must exist.
	must.ErrorContains(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 9, testCustomObject("db.example.com", "Postgres", "default", "a"), 0), "not found")

	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("db.example.com", "Postgres", true)))
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("db.example.com", "PostgresX", true)))

	ws := memdb.NewWatchSet()
	_, err := store.CustomObjectByName(ws, "db.example.com", "Postgres", "default", "a")
	must.NoError(t, err)

	a := testCustomObject("db.example.com", "Postgres", "default", "a")
	a.Labels = map[string]string{"team": "finance"}
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 11, a, 0))
	must.True(t, watchFired(ws))
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 12, testCustomObject("db.example.com", "Postgres", "prod", "a"), 0))
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 13, testCustomObject("db.example.com", "Postgres", "default", "b"), 0))
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 14, testCustomObject("db.example.com", "PostgresX", "default", "c"), 0))

	// Create with an expected index, or a second create, conflicts.
	err = store.UpsertCustomObject(structs.MsgTypeTestSetup, 15, testCustomObject("db.example.com", "Postgres", "default", "new"), 5)
	must.True(t, structs.IsObjectConflictError(err))
	err = store.UpsertCustomObject(structs.MsgTypeTestSetup, 15, testCustomObject("db.example.com", "Postgres", "default", "a"), 0)
	must.True(t, structs.IsObjectConflictError(err))

	// Update with the right index keeps the create index.
	update := testCustomObject("db.example.com", "Postgres", "default", "a")
	update.Generation = 2
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 16, update, 11))
	got, err := store.CustomObjectByName(nil, "db.example.com", "Postgres", "default", "a")
	must.NoError(t, err)
	must.Eq(t, uint64(11), got.CreateIndex)
	must.Eq(t, uint64(16), got.ModifyIndex)
	must.Eq(t, uint64(2), got.Generation)

	// A stale index conflicts.
	err = store.UpsertCustomObject(structs.MsgTypeTestSetup, 17, testCustomObject("db.example.com", "Postgres", "default", "a"), 11)
	must.True(t, structs.IsObjectConflictError(err))

	iter, err := store.CustomObjects(nil, "db.example.com", "Postgres", "default", SortDefault)
	must.Eq(t, []string{"default/a", "default/b"}, countObjects(t, iter, err))
	iter, err = store.CustomObjects(nil, "db.example.com", "Postgres", structs.AllNamespacesSentinel, SortDefault)
	must.Eq(t, []string{"default/a", "default/b", "prod/a"}, countObjects(t, iter, err))
	iter, err = store.CustomObjects(nil, "db.example.com", "PostgresX", structs.AllNamespacesSentinel, SortDefault)
	must.Eq(t, []string{"default/c"}, countObjects(t, iter, err))

	// The update removed the label.
	iter, err = store.CustomObjectsByLabel(nil, "team", "finance", SortDefault)
	must.SliceEmpty(t, countObjects(t, iter, err))
	labeled := testCustomObject("db.example.com", "Postgres", "default", "b")
	labeled.Labels = map[string]string{"team": "finance"}
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 18, labeled, 13))
	iter, err = store.CustomObjectsByLabel(nil, "team", "finance", SortDefault)
	must.Eq(t, []string{"default/b"}, countObjects(t, iter, err))

	iter, err = store.CustomObjectsByNamespace(nil, "prod")
	must.Eq(t, []string{"prod/a"}, countObjects(t, iter, err))

	// Delete: a stale index conflicts, 0 deletes unconditionally.
	err = store.DeleteCustomObject(structs.MsgTypeTestSetup, 19, "db.example.com", "Postgres", "default", "a", 11)
	must.True(t, structs.IsObjectConflictError(err))
	must.NoError(t, store.DeleteCustomObject(structs.MsgTypeTestSetup, 20, "db.example.com", "Postgres", "default", "a", 16))
	must.NoError(t, store.DeleteCustomObject(structs.MsgTypeTestSetup, 21, "db.example.com", "Postgres", "prod", "a", 0))
	must.ErrorContains(t, store.DeleteCustomObject(structs.MsgTypeTestSetup, 22, "db.example.com", "Postgres", "prod", "a", 0), "not found")

	idx, err := store.Index(TableCustomObjects)
	must.NoError(t, err)
	must.Eq(t, uint64(21), idx)
}

func TestStateStore_DeleteNamespaceWithCustomObjects(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	ns := mock.Namespace()
	must.NoError(t, store.UpsertNamespaces(10, []*structs.Namespace{ns}))
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 11, testObjectKind("db.example.com", "Postgres", true)))
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 12, testCustomObject("db.example.com", "Postgres", ns.Name, "a"), 0))

	must.ErrorContains(t, store.DeleteNamespaces(13, []string{ns.Name}), "custom object")

	must.NoError(t, store.DeleteCustomObject(structs.MsgTypeTestSetup, 14, "db.example.com", "Postgres", ns.Name, "a", 0))
	must.NoError(t, store.DeleteNamespaces(15, []string{ns.Name}))
}

func TestStateStore_CustomObjectEvents(t *testing.T) {
	ci.Parallel(t)
	store := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer store.StopEventBroker()

	must.NoError(t, store.UpsertObjectKind(structs.ObjectKindUpsertRequestType, 10, testObjectKind("db.example.com", "Postgres", true)))
	must.NoError(t, store.UpsertCustomObject(structs.CustomObjectUpsertRequestType, 11, testCustomObject("db.example.com", "Postgres", "default", "a"), 0))

	status := testCustomObject("db.example.com", "Postgres", "default", "a")
	status.Status = map[string]any{"observedGeneration": float64(1)}
	must.NoError(t, store.UpsertCustomObject(structs.CustomObjectUpsertRequestType, 12, status, 11))

	spec := status.Copy()
	spec.Spec = map[string]any{"replicas": float64(2)}
	spec.Generation = 2
	must.NoError(t, store.UpsertCustomObject(structs.CustomObjectUpsertRequestType, 13, spec, 12))

	must.NoError(t, store.DeleteCustomObject(structs.CustomObjectDeleteRequestType, 14, "db.example.com", "Postgres", "default", "a", 0))
	must.NoError(t, store.DeleteObjectKind(structs.ObjectKindDeleteRequestType, 15, "db.example.com", "Postgres"))

	events := WaitForEvents(t, store, 10, 6, time.Second)
	must.Len(t, 6, events)
	for _, e := range events {
		must.Eq(t, structs.TopicCustomObject, e.Topic)
		must.Eq(t, []string{"db.example.com/Postgres"}, e.FilterKeys)
	}
	must.Eq(t, structs.TypeObjectKindUpserted, events[0].Type)
	must.Eq(t, "db.example.com/Postgres", events[0].Key)
	must.Eq(t, structs.TypeCustomObjectUpserted, events[1].Type)
	must.Eq(t, "db.example.com/Postgres/a", events[1].Key)
	must.Eq(t, "default", events[1].Namespace)
	must.Eq(t, structs.TypeCustomObjectStatusUpdated, events[2].Type)
	must.Eq(t, structs.TypeCustomObjectUpserted, events[3].Type)
	must.Eq(t, uint64(2), events[3].Payload.(*structs.CustomObjectEvent).Object.Generation)
	must.Eq(t, structs.TypeCustomObjectDeleted, events[4].Type)
	must.Eq(t, structs.TypeObjectKindDeleted, events[5].Type)
}
