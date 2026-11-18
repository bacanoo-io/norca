// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package objectgc

import (
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
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

	l        sync.Mutex
	index    uint64
	stopped  []string
	deleted  []string
	released []string
	keptJobs []string
}

// guardedLabel marks the objects of a test whose deletion is guarded.
const guardedLabel = "guarded"

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, store: state.TestStateStore(t), now: time.Unix(1_700_000_000, 0), index: 100}
	h.gc = New(hclog.NewNullLogger(), DefaultConfig(), &Deps{
		State: func() *state.StateStore { return h.store },
		DeleteObject: func(obj *structs.CustomObject, approved []string) error {
			h.l.Lock()
			h.deleted = append(h.deleted, obj.Name)
			h.l.Unlock()
			if len(obj.Finalizers) > 0 {
				marked := obj.Copy()
				marked.DeletionTime = h.now.UnixNano()
				marked.DeletionPropagation = structs.ObjectPropagationBackground
				marked.DeletionApproved = approved
				return h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), marked, obj.ModifyIndex)
			}
			return h.removeApproved(obj, structs.ObjectPropagationBackground, approved)
		},
		Guarded: func(obj *structs.CustomObject) bool { return obj.Labels[guardedLabel] == "true" },
		ReleaseObject: func(obj *structs.CustomObject, kindRef, namespace, name string) error {
			h.l.Lock()
			h.released = append(h.released, obj.Name)
			h.l.Unlock()
			released := obj.Copy()
			released.Owners = slices.DeleteFunc(released.Owners, func(r *structs.ObjectOwnerReference) bool {
				return r.Kind == kindRef && r.Namespace == namespace && r.Name == name
			})
			return h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), released, obj.ModifyIndex)
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
		KeepJob: func(job *structs.Job, kindRef, namespace, name string) {
			h.l.Lock()
			h.keptJobs = append(h.keptJobs, job.ID+" of "+name)
			h.l.Unlock()
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

// remove removes an object as a deletion does whose caller may stop every
// job there is.
func (h *harness) remove(obj *structs.CustomObject, propagation string) error {
	return h.removeApproved(obj, propagation, h.jobEntries())
}

// jobEntries returns the entries of the jobs that exist, as the list of the
// approved dependents of a deletion has them.
func (h *harness) jobEntries() []string {
	iter, err := h.store.Jobs(nil)
	must.NoError(h.t, err)
	var entries []string
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		job := raw.(*structs.Job)
		entries = append(entries, structs.ObjectApprovedJob(job.Namespace, job.ID, job.CreateIndex))
	}
	return entries
}

// removeApproved removes an object as a deletion does whose caller was
// checked for the guarded dependents with the IDs given.
func (h *harness) removeApproved(obj *structs.CustomObject, propagation string, approved []string) error {
	return h.store.DeleteCustomObjectRequest(structs.MsgTypeTestSetup, h.next(), &structs.CustomObjectDeleteStateRequest{
		Group: obj.Group, Kind: obj.Kind, Namespace: obj.Namespace, Name: obj.Name,
		ExpectedModifyIndex: obj.ModifyIndex, Propagation: propagation, DeleteTime: h.now.UnixNano(),
		ApprovedDependents: approved,
	})
}

// guarded creates an object whose deletion is guarded.
func (h *harness) guarded(name string, owners ...string) *structs.CustomObject {
	obj := h.object(name, owners...).Copy()
	obj.Labels = map[string]string{guardedLabel: "true"}
	must.NoError(h.t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), obj, obj.ModifyIndex))
	return h.get(name)
}

// update writes a changed copy of an object.
func (h *harness) update(name string, change func(obj *structs.CustomObject)) *structs.CustomObject {
	obj := h.get(name).Copy()
	change(obj)
	must.NoError(h.t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), obj, obj.ModifyIndex))
	return h.get(name)
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
	// so late dependents are collected too. A late job is not: nobody was
	// checked for the right to stop it.
	h.object("late-child", "owner")
	h.job("late", "owner")
	must.True(t, h.collect())
	must.Nil(t, h.get("late-child"))
	must.False(t, h.getJob("late").Stop)

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

// TestCollector_RetentionIsNotPendingWork checks that a tombstone that only
// waits for its retention does not keep the collector on its short
// interval: with a long retention every deletion would otherwise make the
// leader read all objects and jobs every few seconds until it ends. The
// tombstone is removed by a pass after the retention.
func TestCollector_RetentionIsNotPendingWork(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	owner := h.object("owner")
	must.NoError(t, h.remove(owner, structs.ObjectPropagationBackground))
	must.False(t, h.collect(), must.Sprint("a tombstone waiting for its retention is pending work"))
	must.Eq(t, []string{"id-owner"}, h.tombstones())

	h.now = h.now.Add(DefaultConfig().TombstoneRetention + time.Second)
	must.False(t, h.collect())
	must.SliceEmpty(t, h.tombstones())
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
	marked.DeletionApproved = h.jobEntries()
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

// An object that owns itself, and objects that own each other, finish a
// foreground deletion (review finding SRV-29).
func TestCollector_ForegroundOwnerCycle(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	mark := func(name string) {
		obj := h.get(name)
		marked := obj.Copy()
		marked.DeletionTime = h.now.UnixNano()
		marked.DeletionPropagation = structs.ObjectPropagationForeground
		must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, h.next(), marked, obj.ModifyIndex))
	}

	h.object("self", "self")
	mark("self")
	h.collect()
	must.Nil(t, h.get("self"), must.Sprint("the object that owns itself"))

	h.object("a", "b")
	h.object("b", "a")
	h.object("child", "a")
	mark("a")
	mark("b")
	for i := 0; i < 3; i++ {
		h.collect()
	}
	must.Nil(t, h.get("a"))
	must.Nil(t, h.get("b"))
	must.Nil(t, h.get("child"))

	// A cycle of which only one object is deleted: the other one is its
	// dependent and goes first.
	h.object("c", "d")
	h.object("d", "c")
	mark("c")
	h.collect()
	must.Nil(t, h.get("d"))
	h.collect()
	must.Nil(t, h.get("c"))
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

// A dependent whose deletion is guarded is deleted with its owner only if
// the deletion of the owner names it: the collector has no caller, and the
// list says for which dependents the deleter of the owner was checked. Any
// other guarded dependent is let go of: it stays, without its reference to
// the owner (review finding SEC-8).
func TestCollector_GuardedDependents(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	kept := func(name string) {
		t.Helper()
		obj := h.get(name)
		must.NotNil(t, obj, must.Sprintf("%s was deleted", name))
		must.False(t, obj.Deleting(), must.Sprintf("%s is marked for deletion", name))
		must.Len(t, 0, obj.Owners, must.Sprintf("%s still has its owner", name))
	}

	// Background: the approved dependent and the one that is not guarded
	// go, the other guarded one stays.
	owner := h.object("owner")
	h.guarded("approved", "owner")
	h.guarded("unapproved", "owner")
	h.object("plain", "owner")
	must.NoError(t, h.removeApproved(owner, structs.ObjectPropagationBackground, []string{"id-approved"}))
	h.collect()
	must.Nil(t, h.get("approved"))
	must.Nil(t, h.get("plain"))
	kept("unapproved")
	h.collect()
	kept("unapproved")
	must.Eq(t, []string{"unapproved"}, h.released)

	// The list holds through an object in between, which the collector
	// deletes itself, with finalizers (marked) or without (removed).
	for _, between := range []string{"removed", "marked"} {
		top := h.object("top-" + between)
		h.object(between, "top-"+between)
		if between == "marked" {
			h.update(between, func(obj *structs.CustomObject) { obj.Finalizers = []string{"example.com/hold"} })
		}
		h.guarded("deep-"+between, between)
		h.guarded("deep-late-"+between, between)
		must.NoError(t, h.removeApproved(top, structs.ObjectPropagationBackground, []string{"id-deep-" + between}))
		h.collect()
		if between == "marked" {
			must.Eq(t, []string{"id-deep-marked"}, h.get(between).DeletionApproved)
			h.update(between, func(obj *structs.CustomObject) { obj.Finalizers = nil })
			h.collect()
		}
		h.collect()
		must.Nil(t, h.get(between))
		must.Nil(t, h.get("deep-"+between), must.Sprintf("deep-%s", between))
		kept("deep-late-" + between)
	}

	// An owner that is marked for deletion: the list of its mark goes into
	// its tombstone, whoever removes the object.
	h.object("waiting")
	h.guarded("with-waiting", "waiting")
	h.update("waiting", func(obj *structs.CustomObject) {
		obj.Finalizers = []string{"example.com/hold"}
		obj.DeletionTime = h.now.UnixNano()
		obj.DeletionPropagation = structs.ObjectPropagationBackground
		obj.DeletionApproved = []string{"id-with-waiting"}
	})
	must.True(t, h.collect())
	must.NotNil(t, h.get("with-waiting"))
	h.update("waiting", func(obj *structs.CustomObject) { obj.Finalizers = nil })
	h.collect()
	h.collect()
	must.Nil(t, h.get("waiting"))
	must.Nil(t, h.get("with-waiting"))

	// Foreground: a guarded dependent that is not approved does not hold
	// its owner, and stays.
	h.object("front")
	h.guarded("behind", "front")
	h.update("front", func(obj *structs.CustomObject) {
		obj.DeletionTime = h.now.UnixNano()
		obj.DeletionPropagation = structs.ObjectPropagationForeground
	})
	h.collect()
	h.collect()
	must.Nil(t, h.get("front"))
	kept("behind")

	// A guarded dependent with another owner alive is left alone, with both
	// references.
	gone := h.object("gone")
	h.object("alive")
	h.guarded("shared", "gone", "alive")
	must.NoError(t, h.remove(gone, structs.ObjectPropagationBackground))
	h.collect()
	must.Len(t, 2, h.get("shared").Owners)
}

// A dependent job is stopped with its owner only if the deletion of the
// owner names it: stopping a job needs a right, and the list says for which
// jobs the deleter of the owner was checked. Any other job keeps running,
// untouched, and does not hold its owner.
func TestCollector_Jobs(t *testing.T) {
	ci.Parallel(t)
	h := newHarness(t)

	entry := func(id string) string {
		job := h.getJob(id)
		return structs.ObjectApprovedJob(job.Namespace, job.ID, job.CreateIndex)
	}
	running := func(id string) {
		t.Helper()
		job := h.getJob(id)
		must.False(t, job.Stop, must.Sprintf("the job %s was stopped", id))
		must.MapContainsKey(t, job.Meta, structs.JobMetaOwner, must.Sprintf("the meta of the job %s was changed", id))
	}

	// Background: the job the deleter was checked for stops; the other one,
	// and one that is attached afterwards, keep running. That is told once.
	owner := h.object("owner")
	h.job("approved", "owner")
	h.job("unapproved", "owner")
	must.NoError(t, h.removeApproved(owner, structs.ObjectPropagationBackground, []string{entry("approved")}))
	h.collect()
	h.job("attached-later", "owner")
	h.collect()
	h.collect()
	must.True(t, h.getJob("approved").Stop)
	running("unapproved")
	running("attached-later")
	must.Eq(t, []string{"unapproved of owner", "attached-later of owner"}, h.keptJobs)

	// A job that is purged and registered again under its ID is another
	// job: the entry of the first one is not its entry.
	again := h.object("again")
	h.job("reborn", "again")
	first := entry("reborn")
	must.NoError(t, h.store.DeleteJob(h.next(), "default", "reborn"))
	h.job("reborn", "again")
	must.NotEq(t, first, entry("reborn"))
	must.NoError(t, h.removeApproved(again, structs.ObjectPropagationBackground, []string{first}))
	h.collect()
	running("reborn")

	// The list holds through an object in between.
	top := h.object("top")
	h.object("middle", "top")
	h.job("deep", "middle")
	h.job("deep-unapproved", "middle")
	must.NoError(t, h.removeApproved(top, structs.ObjectPropagationBackground, []string{entry("deep")}))
	h.collect()
	h.collect()
	must.Nil(t, h.get("middle"))
	must.True(t, h.getJob("deep").Stop)
	running("deep-unapproved")

	// Foreground: a job that is not approved does not hold its owner.
	h.object("front")
	h.job("behind", "front")
	h.update("front", func(obj *structs.CustomObject) {
		obj.DeletionTime = h.now.UnixNano()
		obj.DeletionPropagation = structs.ObjectPropagationForeground
	})
	h.collect()
	must.Nil(t, h.get("front"))
	running("behind")

	// A deletion by the servers names nothing.
	served := h.object("served")
	h.job("of-served", "served")
	must.NoError(t, h.removeApproved(served, structs.ObjectPropagationBackground, nil))
	h.collect()
	running("of-served")
}
