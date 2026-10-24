// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package objectgc

import (
	"sort"
	"sync"
	"testing"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// harness applies the collector's actions to a state store, like the
// server does through Raft.
type harness struct {
	t     *testing.T
	store *state.StateStore
	gc    *Collector
	now   time.Time

	l       sync.Mutex
	index   uint64
	stopped []string
	deleted []string
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, store: state.TestStateStore(t), now: time.Unix(1_700_000_000, 0), index: 100}
	h.gc = New(hclog.NewNullLogger(), DefaultConfig(), &Deps{
		State: func() *state.StateStore { return h.store },
		DeleteObject: func(obj *structs.CustomObject) error {
			h.l.Lock()
			h.deleted = append(h.deleted, obj.Name)
			h.l.Unlock()
			if len(obj.Finalizers) > 0 {
				marked := obj.Copy()
				marked.DeletionTime = h.now.UnixNano()
				marked.DeletionPropagation = structs.ObjectPropagationBackground
				return h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), marked, obj.ModifyIndex)
			}
			return h.remove(obj, structs.ObjectPropagationBackground)
		},
		RemoveObject: h.remove,
		StopJob: func(namespace, id string) error {
			h.l.Lock()
			h.stopped = append(h.stopped, id)
			h.l.Unlock()
			job, err := h.store.JobByID(nil, namespace, id)
			must.NoError(t, err)
			job = job.Copy()
			job.Stop = true
			return h.store.UpsertJob(structs.MsgTypeTestSetup, h.next(), nil, job)
		},
		DeleteTombstones: func(ids []string) error {
			return h.store.ApplyObjectGC(structs.MsgTypeTestSetup, h.next(), &structs.ObjectGCRequest{DeleteTombstones: ids})
		},
		Now: func() time.Time { return h.now },
	})
	must.NoError(t, h.store.UpsertObjectKind(structs.MsgTypeTestSetup, h.next(),
		&structs.ObjectKind{Group: "example.com", Kind: "Thing", Namespaced: true, Schema: `{"type":"object"}`}))
	return h
}

func (h *harness) next() uint64 {
	h.l.Lock()
	defer h.l.Unlock()
	h.index++
	return h.index
}

func (h *harness) remove(obj *structs.CustomObject, propagation string) error {
	return h.store.DeleteCustomObjectRequest(structs.MsgTypeTestSetup, h.next(), &structs.CustomObjectDeleteStateRequest{
		Group: obj.Group, Kind: obj.Kind, Namespace: obj.Namespace, Name: obj.Name,
		ExpectedModifyIndex: obj.ModifyIndex, Propagation: propagation, DeleteTime: h.now.UnixNano(),
	})
}

func (h *harness) object(name string, owners ...string) *structs.CustomObject {
	obj := &structs.CustomObject{ID: "id-" + name, Group: "example.com", Kind: "Thing", Namespace: "default",
		Name: name, Generation: 1, Spec: map[string]any{}}
	for _, o := range owners {
		obj.Owners = append(obj.Owners, &structs.ObjectOwnerReference{Kind: "example.com/Thing", Namespace: "default", Name: o})
	}
	must.NoError(h.t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), obj, 0))
	return h.get(name)
}

func (h *harness) get(name string) *structs.CustomObject {
	obj, err := h.store.CustomObjectByName(nil, "example.com", "Thing", "default", name)
	must.NoError(h.t, err)
	return obj
}

func (h *harness) job(id, owner string) *structs.Job {
	job := mock.Job()
	job.ID = id
	job.Meta = map[string]string{
		structs.JobMetaOwner:     structs.ObjectOwnerMetaValue("example.com", "Thing", "default", owner),
		structs.JobMetaOwnerKind: "example.com/Thing",
	}
	must.NoError(h.t, h.store.UpsertJob(structs.MsgTypeTestSetup, h.next(), nil, job))
	return job
}

func (h *harness) getJob(id string) *structs.Job {
	job, err := h.store.JobByID(nil, "default", id)
	must.NoError(h.t, err)
	return job
}

func (h *harness) collect() bool {
	pending, err := h.gc.Collect(memdb.NewWatchSet(), h.store)
	must.NoError(h.t, err)
	return pending
}

func (h *harness) tombstones() []string {
	iter, err := h.store.ObjectTombstones(nil)
	must.NoError(h.t, err)
	var ids []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		ids = append(ids, raw.(*structs.ObjectTombstone).ID)
	}
	sort.Strings(ids)
	return ids
}

func TestCollector_Background(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	owner := h.object("owner")
	h.object("child", "owner")
	fin := h.object("child-fin", "owner")
	fin = fin.Copy()
	fin.Finalizers = []string{"example.com/cleanup"}
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), fin, h.get("child-fin").ModifyIndex))
	h.object("unrelated")
	h.job("owned", "owner")
	h.job("other", "unrelated")

	// Nothing to do.
	must.False(t, h.collect())

	must.NoError(t, h.remove(owner, structs.ObjectPropagationBackground))
	must.Eq(t, []string{"id-owner"}, h.tombstones())

	must.True(t, h.collect())
	must.Nil(t, h.get("child"))
	must.True(t, h.getJob("owned").Stop)
	must.False(t, h.getJob("other").Stop)
	must.NotNil(t, h.get("unrelated"))

	// The dependent with a finalizer is only marked for deletion.
	must.True(t, h.get("child-fin").Deleting())

	// The deleted child left its own tombstone, for its dependents.
	must.Eq(t, []string{"id-child", "id-owner"}, h.tombstones())

	// The tombstones are kept for the retention period after the deletion,
	// so late dependents are collected too.
	h.job("late", "owner")
	must.True(t, h.collect())
	must.True(t, h.getJob("late").Stop)

	// Once the finalizer is gone and the retention passed, everything is
	// cleaned up.
	cur := h.get("child-fin").Copy()
	cur.Finalizers = nil
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), cur, h.get("child-fin").ModifyIndex))
	h.now = h.now.Add(DefaultConfig().TombstoneRetention + time.Second)
	h.collect()
	must.Nil(t, h.get("child-fin"))
	h.now = h.now.Add(DefaultConfig().TombstoneRetention + time.Second)
	must.False(t, h.collect())
	must.Eq(t, 0, len(h.tombstones()))
}

func TestCollector_Foreground(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	owner := h.object("owner")
	h.object("child", "owner")
	h.job("owned", "owner")

	marked := owner.Copy()
	marked.DeletionTime = h.now.UnixNano()
	marked.DeletionPropagation = structs.ObjectPropagationForeground
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), marked, owner.ModifyIndex))

	// The dependents are deleted first; the owner stays while the job is
	// not dead.
	must.True(t, h.collect())
	must.Nil(t, h.get("child"))
	must.True(t, h.getJob("owned").Stop)
	must.NotNil(t, h.get("owner"))
	must.True(t, h.collect())
	must.NotNil(t, h.get("owner"))

	// The deregistration's evaluation completes: the stopped job is dead.
	eval := mock.Eval()
	eval.JobID = "owned"
	eval.Status = structs.EvalStatusComplete
	must.NoError(t, h.store.UpsertEvals(structs.MsgTypeTestSetup, h.next(), []*structs.Evaluation{eval}))
	must.Eq(t, structs.JobStatusDead, h.getJob("owned").Status)

	h.collect()
	must.Nil(t, h.get("owner"))
	must.SliceContains(t, h.tombstones(), "id-owner")
}

func TestCollector_ForegroundWaitsForFinalizers(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	owner := h.object("owner")
	marked := owner.Copy()
	marked.Finalizers = []string{"example.com/cleanup"}
	marked.DeletionTime = h.now.UnixNano()
	marked.DeletionPropagation = structs.ObjectPropagationForeground
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), marked, owner.ModifyIndex))

	must.True(t, h.collect())
	must.NotNil(t, h.get("owner"))

	cur := h.get("owner").Copy()
	cur.Finalizers = nil
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), cur, h.get("owner").ModifyIndex))
	h.collect()
	must.Nil(t, h.get("owner"))
}

func TestCollector_OrphanAndAdoption(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	// Orphan: no tombstone, the job stays.
	orphaner := h.object("orphaner")
	h.job("kept", "orphaner")
	must.NoError(t, h.remove(orphaner, structs.ObjectPropagationOrphan))
	must.False(t, h.collect())
	must.False(t, h.getJob("kept").Stop)

	// A re-created owner adopts the dependents of the deleted one.
	owner := h.object("owner")
	h.job("adopted", "owner")
	must.NoError(t, h.remove(owner, structs.ObjectPropagationBackground))
	h.object("owner")
	h.collect()
	must.False(t, h.getJob("adopted").Stop)
}

func TestCollector_MultipleOwners(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	a := h.object("a")
	h.object("b")
	h.object("shared", "a", "b")

	must.NoError(t, h.remove(a, structs.ObjectPropagationBackground))
	h.collect()
	must.NotNil(t, h.get("shared"))

	// When the last owner is gone, the dependent is collected.
	must.NoError(t, h.remove(h.get("b"), structs.ObjectPropagationBackground))
	h.collect()
	must.Nil(t, h.get("shared"))
}

func TestCollector_Run(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)
	cfg := DefaultConfig()
	cfg.Interval = 10 * time.Millisecond
	h.gc.SetConfig(cfg)
	h.gc.deps.Now = time.Now
	h.gc.now = time.Now

	owner := h.object("owner")
	h.job("owned", "owner")

	h.gc.SetEnabled(true)
	defer h.gc.SetEnabled(false)
	must.True(t, h.gc.Running())

	// A deletion wakes the collector up.
	must.NoError(t, h.remove(owner, structs.ObjectPropagationBackground))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return h.getJob("owned").Stop }),
		wait.Timeout(10*time.Second),
		wait.Gap(10*time.Millisecond),
	))
}
