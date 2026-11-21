// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"slices"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/objectgc"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// deleteReply deletes an object of the default namespace and returns the
// reply.
func (c *certTest) deleteReply(group, kind, name, propagation string, dryRun bool) (*structs.CustomObjectWriteResponse, error) {
	var resp structs.CustomObjectWriteResponse
	err := c.s.RPC("CustomObject.Delete", &structs.CustomObjectDeleteRequest{
		Group: group, Kind: kind, Name: name, Propagation: propagation, DryRun: dryRun,
		WriteRequest: c.write("default"),
	}, &resp)
	return &resp, err
}

// tombstone returns the tombstone of an object of the default namespace, or
// nil.
func (c *certTest) tombstone(group, kind, name string) *structs.ObjectTombstone {
	c.t.Helper()
	iter, err := c.s.State().ObjectTombstones(nil)
	must.NoError(c.t, err)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		t := raw.(*structs.ObjectTombstone)
		if t.Group == group && t.Kind == kind && t.Namespace == "default" && t.Name == name {
			return t
		}
	}
	return nil
}

// letsGoTokens are the administrator of the Databases (and of the jobs of
// the namespace), and a controller that makes Hooks the dependents of
// Databases: it has the finalizers of the Databases, and may not delete one.
func letsGoTokens(t *testing.T, c *certTest) (victim, controller *certTest) {
	victim = c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1001, "victim", `namespace "default" {
  object "example.com/Database" { capabilities = ["read", "write", "manage-finalizers"] }
  capabilities = ["submit-job", "read-job", "list-jobs"]
}`))
	controller = c.as(mock.CreatePolicyAndToken(t, c.s.State(), 1003, "controller", `namespace "default" {
  object "example.com/Hook" { capabilities = ["read", "write", "manage-finalizers"] }
  object "example.com/Database" { capabilities = ["manage-finalizers"] }
}`))
	return victim, controller
}

func ownedHook(name, owner string, finalizers ...string) *structs.CustomObject {
	return &structs.CustomObject{Group: testOwnerGroup, Kind: testOwnerMarkHookKind, Name: name,
		Spec: map[string]any{}, Finalizers: finalizers,
		Owners: []*structs.ObjectOwnerReference{ownedBy(testOwnerGroup, testOwnerKind, owner)}}
}

// A foreground deletion waits for its dependents, and a dependent with a
// finalizer that nobody removes holds it for good. A repeated deletion in the
// orphan mode lets go of the dependents (review finding JOG-21, the way out
// of its scenario): the owner is removed, the dependent the collector had
// marked stays as it is, held by its finalizer, and the dependent the
// collector had removed is gone. Before the fix the repeated deletion changed
// nothing, and only the owner of the finalizer or a management token ended
// the deletion.
func TestObjectDelete_OrphanLetsGoOfAForegroundDeletion(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := ownerMarkServer(t)
	defer cleanup()
	victim, controller := letsGoTokens(t, c)
	// The tombstones are looked at: they stay for the test.
	c.s.objectGC.SetConfig(&objectgc.Config{Interval: 50 * time.Millisecond, IdleInterval: time.Second, TombstoneRetention: time.Hour})

	const fin = "example.com/never-removed"
	must.NoError(t, victim.applyOwned(testOwnerGroup, testOwnerKind, "shop", map[string]any{}))
	_, err := controller.applyObject(ownedHook("held", "shop", fin))
	must.NoError(t, err)
	_, err = controller.applyObject(ownedHook("free", "shop"))
	must.NoError(t, err)

	// The foreground deletion: the collector removes the dependent without
	// a finalizer, marks the one with it, and the owner waits.
	must.NoError(t, victim.deleteWith(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationForeground))
	c.waitGone(testOwnerGroup, testOwnerMarkHookKind, "free")
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		obj := c.object(testOwnerGroup, testOwnerMarkHookKind, "held")
		return obj != nil && obj.Deleting()
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the dependent is marked for deletion"))
	must.Wait(t, wait.ContinualSuccess(wait.BoolFunc(func() bool {
		obj := c.object(testOwnerGroup, testOwnerKind, "shop")
		return obj != nil && obj.Deleting()
	}), wait.Timeout(time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("the owner stays in deletion"))
	marked := c.object(testOwnerGroup, testOwnerKind, "shop")
	held := c.object(testOwnerGroup, testOwnerMarkHookKind, "held")

	// The finalizer of the dependent is not the administrator's to remove.
	err = victim.objectFinalizers(testOwnerMarkHookKind, "held", nil, []string{fin})
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// A repeated deletion in a mode that takes the dependents changes
	// nothing.
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground} {
		resp, err := victim.deleteReply(testOwnerGroup, testOwnerKind, "shop", mode, false)
		must.NoError(t, err)
		must.True(t, resp.Unchanged, must.Sprintf("mode %q", mode))
		must.Eq(t, marked.ModifyIndex, resp.Index)
	}
	must.Eq(t, marked.ModifyIndex, c.object(testOwnerGroup, testOwnerKind, "shop").ModifyIndex)

	// Letting go takes the right to delete the owner.
	_, err = controller.deleteReply(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationOrphan, false)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// A dry run tells what would be done, and does nothing.
	resp, err := victim.deleteReply(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationOrphan, true)
	must.NoError(t, err)
	must.False(t, resp.Unchanged)
	must.False(t, resp.Object.Deleting(), must.Sprint("the dry run reply of a removal is an object marked for deletion (review finding MUK-31)"))
	must.Eq(t, marked.ModifyIndex, c.object(testOwnerGroup, testOwnerKind, "shop").ModifyIndex)

	// The repeated deletion in the orphan mode: the owner has no finalizer
	// of its own, so it is removed at once, by one write.
	resp, err = victim.deleteReply(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationOrphan, false)
	must.NoError(t, err)
	must.False(t, resp.Unchanged, must.Sprint("a deletion in the orphan mode changed nothing of a foreground deletion"))
	must.Nil(t, c.object(testOwnerGroup, testOwnerKind, "shop"))
	// The reply tells a removal as the reply of a first deletion does: the
	// object without the mark of a deletion (review finding MUK-31). With
	// the mark of the foreground deletion in it the caller read "marked for
	// deletion (foreground)" of an object that was gone.
	must.NotNil(t, resp.Object)
	must.False(t, resp.Object.Deleting(), must.Sprint("the reply of a removal is an object marked for deletion"))
	must.Eq(t, "", resp.Object.DeletionPropagation)
	must.SliceEmpty(t, resp.Object.DeletionApproved)
	must.Eq(t, marked.ID, resp.Object.ID)
	tomb := c.tombstone(testOwnerGroup, testOwnerKind, "shop")
	must.NotNil(t, tomb)
	must.Eq(t, structs.ObjectPropagationOrphan, tomb.Propagation)
	must.Eq(t, resp.Index, tomb.CreateIndex)

	// What the collector did before stays done: the dependent it marked is
	// still marked and held by its finalizer, with its reference to the
	// owner, and nothing was written to it.
	must.Wait(t, wait.ContinualSuccess(wait.BoolFunc(func() bool {
		obj := c.object(testOwnerGroup, testOwnerMarkHookKind, "held")
		return obj != nil && obj.ModifyIndex == held.ModifyIndex
	}), wait.Timeout(time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("the dependent stays as it was"))
	after := c.object(testOwnerGroup, testOwnerMarkHookKind, "held")
	must.True(t, after.Deleting())
	must.Eq(t, []string{fin}, after.Finalizers)
	must.True(t, structs.ObjectOwnedBy(after, testOwnerGroup, testOwnerKind, "default", "shop"))
	must.Nil(t, c.object(testOwnerGroup, testOwnerMarkHookKind, "free"))

	// The object is gone: another deletion finds none.
	_, err = victim.deleteReply(testOwnerGroup, testOwnerKind, "shop", structs.ObjectPropagationOrphan, false)
	must.Eq(t, 404, errCode(t, err))

	// The dependent goes when its finalizer does.
	must.NoError(t, controller.objectFinalizers(testOwnerMarkHookKind, "held", nil, []string{fin}))
	c.waitGone(testOwnerGroup, testOwnerMarkHookKind, "held")
}

// An object with a finalizer of its own that is being deleted in the
// background mode has taken no dependent yet. A repeated deletion in the
// orphan mode changes the mode of the mark by one write: the object and the
// job that depend on it stay when it is removed. The mode is not changed
// back.
func TestObjectDelete_OrphanLetsGoOfABackgroundDeletion(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := ownerMarkServer(t)
	defer cleanup()
	victim, controller := letsGoTokens(t, c)
	// The tombstones are looked at: they stay for the test.
	c.s.objectGC.SetConfig(&objectgc.Config{Interval: 50 * time.Millisecond, IdleInterval: time.Second, TombstoneRetention: time.Hour})

	const fin = "example.com/cleanup"
	must.NoError(t, victim.applyOwned(testOwnerGroup, testOwnerKind, "site", map[string]any{}))
	must.NoError(t, victim.objectFinalizers(testOwnerKind, "site", []string{fin}, nil))
	_, err := controller.applyObject(ownedHook("kept", "site"))
	must.NoError(t, err)
	must.NoError(t, victim.squatJob("default", "site-app", "site"))
	job := c.jobIn("default", "site-app")

	// The deletion in the default mode marks the object: its finalizer holds
	// it. The caller was checked for the job.
	resp, err := victim.deleteReply(testOwnerGroup, testOwnerKind, "site", "", false)
	must.NoError(t, err)
	marked := c.object(testOwnerGroup, testOwnerKind, "site")
	must.True(t, marked.Deleting())
	must.Eq(t, structs.ObjectPropagationBackground, marked.DeletionPropagation)
	must.Eq(t, []string{structs.ObjectApprovedJob("default", "site-app", job.CreateIndex)}, marked.DeletionApproved)
	must.Eq(t, resp.Index, marked.ModifyIndex)

	// The repeated deletion in the orphan mode writes the mark once: the
	// mode, and nothing approved to take. The time of the deletion stays.
	before := c.s.raft.LastIndex()
	resp, err = victim.deleteReply(testOwnerGroup, testOwnerKind, "site", structs.ObjectPropagationOrphan, false)
	written := c.s.raft.LastIndex() - before
	must.NoError(t, err)
	must.False(t, resp.Unchanged, must.Sprint("a deletion in the orphan mode changed nothing of a background deletion"))
	must.Eq(t, 1, written, must.Sprint("the change is one Raft write"))
	must.Eq(t, structs.ObjectPropagationOrphan, resp.Object.DeletionPropagation)
	let := c.object(testOwnerGroup, testOwnerKind, "site")
	must.Eq(t, structs.ObjectPropagationOrphan, let.DeletionPropagation)
	must.Eq(t, marked.DeletionTime, let.DeletionTime)
	must.SliceEmpty(t, let.DeletionApproved)
	must.Eq(t, []string{fin}, let.Finalizers)
	must.Eq(t, resp.Index, let.ModifyIndex)

	// The mode is not changed back, and a second letting go changes nothing.
	for _, mode := range []string{"", structs.ObjectPropagationBackground, structs.ObjectPropagationForeground, structs.ObjectPropagationOrphan} {
		resp, err := victim.deleteReply(testOwnerGroup, testOwnerKind, "site", mode, false)
		must.NoError(t, err)
		must.True(t, resp.Unchanged, must.Sprintf("mode %q", mode))
	}
	must.Eq(t, let.ModifyIndex, c.object(testOwnerGroup, testOwnerKind, "site").ModifyIndex)

	// The object is removed when its finalizer is gone, and what depends on
	// it stays: the object untouched, with its reference, and the job
	// running.
	kept := c.object(testOwnerGroup, testOwnerMarkHookKind, "kept")
	must.NoError(t, victim.objectFinalizers(testOwnerKind, "site", nil, []string{fin}))
	c.waitGone(testOwnerGroup, testOwnerKind, "site")
	tomb := c.tombstone(testOwnerGroup, testOwnerKind, "site")
	must.NotNil(t, tomb)
	must.Eq(t, structs.ObjectPropagationOrphan, tomb.Propagation)
	must.Wait(t, wait.ContinualSuccess(wait.BoolFunc(func() bool {
		obj := c.object(testOwnerGroup, testOwnerMarkHookKind, "kept")
		return obj != nil && !obj.Deleting() && obj.ModifyIndex == kept.ModifyIndex && !c.jobIn("default", "site-app").Stop
	}), wait.Timeout(time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("the dependents stay"))
	must.True(t, structs.ObjectOwnedBy(c.object(testOwnerGroup, testOwnerMarkHookKind, "kept"),
		testOwnerGroup, testOwnerKind, "default", "site"))
	log, err := c.s.State().ObjectEventLog(nil, testOwnerGroup, testOwnerKind, "default", "site")
	must.NoError(t, err)
	must.True(t, log == nil || !slices.ContainsFunc(log.Events, func(e *structs.ObjectEvent) bool {
		return e.Reason == objectEventDependentKept
	}))
}
