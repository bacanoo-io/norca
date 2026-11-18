// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
)

func TestStateStore_CustomObjectsByOwner(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("example.com", "Thing", true)))
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 11, testObjectKind("example.com", "Fleet", false)))

	owner := func(kind, ns, name string) *structs.ObjectOwnerReference {
		return &structs.ObjectOwnerReference{Kind: kind, Namespace: ns, Name: name}
	}
	write := func(index uint64, name string, owners ...*structs.ObjectOwnerReference) {
		t.Helper()
		obj := testCustomObject("example.com", "Thing", "default", name)
		obj.Owners = owners
		existing, err := store.CustomObjectByName(nil, "example.com", "Thing", "default", name)
		must.NoError(t, err)
		var expected uint64
		if existing != nil {
			expected = existing.ModifyIndex
		}
		must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, index, obj, expected))
	}
	byOwner := func(kindRef, ns, name string) []string {
		t.Helper()
		iter, err := store.CustomObjectsByOwner(nil, kindRef, ns, name)
		return countObjects(t, iter, err)
	}

	write(20, "a", owner("example.com/Thing", "default", "root"))
	write(21, "b", owner("example.com/Thing", "default", "root"), owner("example.com/Fleet", "", "main"),
		owner("example.com/Fleet", "", "main"))
	write(22, "c", owner("example.com/Thing", "other", "root"))
	// An owner that is not an object is not indexed.
	write(23, "d", owner("job", "default", "root"))
	write(24, "e")

	must.Eq(t, []string{"default/a", "default/b"}, byOwner("example.com/Thing", "default", "root"))
	must.Eq(t, []string{"default/b"}, byOwner("example.com/Fleet", "", "main"))
	must.Eq(t, []string{"default/c"}, byOwner("example.com/Thing", "other", "root"))
	must.SliceEmpty(t, byOwner("example.com/Thing", "default", "roo"))
	must.SliceEmpty(t, byOwner("job", "default", "root"))

	// A changed or deleted object leaves the index of its old owners.
	write(30, "a", owner("example.com/Fleet", "", "main"))
	must.NoError(t, store.DeleteCustomObject(structs.MsgTypeTestSetup, 31, "example.com", "Thing", "default", "b", 0))
	must.SliceEmpty(t, byOwner("example.com/Thing", "default", "root"))
	must.Eq(t, []string{"default/a"}, byOwner("example.com/Fleet", "", "main"))
}

func TestStateStore_JobsByOwner(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	const value = "default/crd/example.com/Thing/root"
	write := func(index uint64, id string, meta map[string]string) {
		t.Helper()
		job := mock.Job()
		job.ID, job.Name = id, id
		job.Meta = meta
		must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))
	}
	byOwner := func(value string) []string {
		t.Helper()
		iter, err := store.JobsByOwner(nil, value)
		must.NoError(t, err)
		var ids []string
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			ids = append(ids, raw.(*structs.Job).ID)
		}
		return ids
	}

	write(10, "a", map[string]string{structs.JobMetaOwner: value})
	write(11, "b", map[string]string{structs.JobMetaOwner: value, "other": "x"})
	write(12, "c", map[string]string{structs.JobMetaOwner: value + "-2"})
	write(13, "d", map[string]string{"other": value})
	write(14, "e", nil)

	must.Eq(t, []string{"a", "b"}, byOwner(value))
	must.Eq(t, []string{"c"}, byOwner(value+"-2"))
	must.SliceEmpty(t, byOwner("default/crd/example.com/Thing/roo"))

	// A changed or deleted job leaves the index of its old owner.
	write(20, "a", map[string]string{structs.JobMetaOwner: value + "-2"})
	must.NoError(t, store.DeleteJob(21, "default", "b"))
	must.SliceEmpty(t, byOwner(value))
	must.Eq(t, []string{"a", "c"}, byOwner(value+"-2"))
}

func TestStateStore_CustomObjectsDeleting(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)
	must.NoError(t, store.UpsertObjectKind(structs.MsgTypeTestSetup, 10, testObjectKind("example.com", "Thing", true)))
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 11, testCustomObject("example.com", "Thing", "default", "a"), 0))
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 12, testCustomObject("example.com", "Thing", "default", "b"), 0))

	deleting := func() []string {
		t.Helper()
		iter, err := store.CustomObjectsDeleting(nil)
		return countObjects(t, iter, err)
	}
	must.SliceEmpty(t, deleting())

	// Any change of an object fires the watch of the table.
	ws := memdb.NewWatchSet()
	must.NoError(t, store.WatchCustomObjects(ws))
	marked := testCustomObject("example.com", "Thing", "default", "b")
	marked.DeletionTime = 1000
	must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, 13, marked, 12))
	must.True(t, watchFired(ws))
	must.Eq(t, []string{"default/b"}, deleting())

	must.NoError(t, store.DeleteCustomObject(structs.MsgTypeTestSetup, 14, "example.com", "Thing", "default", "b", 0))
	must.SliceEmpty(t, deleting())
}
