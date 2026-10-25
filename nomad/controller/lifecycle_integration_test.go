// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/shoenig/test/must"
)

// ownedTestJob returns a job without placements owned by obj.
func ownedTestJob(id string, obj *controller.Object) *api.Job {
	job := api.NewServiceJob(id, id, "global", 50)
	job.Datacenters = []string{"dc1"}
	tg := api.NewTaskGroup("g", 0)
	tg.AddTask(api.NewTask("t", "mock_driver"))
	job.AddTaskGroup(tg)
	controller.SetOwner(job, obj)
	return job
}

func TestIntegration_NativeLifecycle(t *testing.T) {
	client := startAgent(t, 100)
	registerWidgetKind(t, client)
	backend := controller.NewBackend(client)
	objects, err := controller.NewNativeObjects(widgetKind, backend)
	must.NoError(t, err)
	ctx := context.Background()

	// Finalizers: the deletion only marks the object until the controller
	// removes its finalizer.
	const fin = "example.com/cleanup"
	obj, err := objects.Create(ctx, &controller.Object{Key: controller.ObjectKey{Namespace: "default", Name: "f"}})
	must.NoError(t, err)
	obj, err = objects.AddFinalizer(ctx, obj, fin)
	must.NoError(t, err)
	must.True(t, obj.HasFinalizer(fin))
	must.NoError(t, objects.Delete(ctx, obj.Key, 0))
	obj, err = objects.Get(ctx, obj.Key)
	must.NoError(t, err)
	must.True(t, obj.Deleting())
	_, err = objects.AddFinalizer(ctx, obj, "example.com/other")
	must.ErrorIs(t, err, controller.ErrConflict)
	gone, err := objects.RemoveFinalizer(ctx, obj, fin)
	must.NoError(t, err)
	must.Len(t, 0, gone.Finalizers)
	_, err = objects.Get(ctx, obj.Key)
	must.ErrorIs(t, err, controller.ErrNotFound)

	// Events: recorded repetitions are counted by the server, and dropped
	// within the recorder's window.
	owner, err := objects.Create(ctx, &controller.Object{Key: controller.ObjectKey{Namespace: "default", Name: "owner"}})
	must.NoError(t, err)
	rec := controller.NewEventRecorder(objects, 0)
	rec.Event(ctx, owner.Key, controller.EventTypeNormal, "Reconciled", "ok")
	rec.Event(ctx, owner.Key, controller.EventTypeNormal, "Reconciled", "ok")
	must.NoError(t, objects.RecordEvent(ctx, owner.Key, controller.EventTypeNormal, "Reconciled", "ok"))
	events, err := objects.Events(ctx, owner.Key)
	must.NoError(t, err)
	must.Len(t, 1, events)
	must.Eq(t, 2, events[0].Count)

	// Owner references: the server's garbage collector stops the owned
	// job and deletes the owned object when the owner is deleted.
	_, _, err = client.Jobs().Register(ownedTestJob("owned-job", owner), nil)
	must.NoError(t, err)
	child := &controller.Object{Key: controller.ObjectKey{Namespace: "default", Name: "child"}}
	controller.SetObjectOwner(child, owner)
	child, err = objects.Create(ctx, child)
	must.NoError(t, err)
	must.True(t, controller.IsObjectOwnedBy(child, owner))
	must.Eq(t, owner.ID, child.Owners[0].ID)

	must.NoError(t, objects.Delete(ctx, owner.Key, 0))
	waitForLong(t, "dependents collected", func() bool {
		job, _, err := client.Jobs().Info("owned-job", nil)
		if err != nil || job.Stop == nil || !*job.Stop {
			return false
		}
		_, err = objects.Get(ctx, child.Key)
		return err != nil
	})

	// Orphan: the job stays.
	orphaner, err := objects.Create(ctx, &controller.Object{Key: controller.ObjectKey{Namespace: "default", Name: "orphaner"}})
	must.NoError(t, err)
	_, _, err = client.Jobs().Register(ownedTestJob("kept-job", orphaner), nil)
	must.NoError(t, err)
	_, _, err = client.Objects().Delete(widgetKind.Group, widgetKind.Kind, "orphaner",
		&api.ObjectWriteOptions{Propagation: api.ObjectPropagationOrphan}, nil)
	must.NoError(t, err)

	// Re-creating the owner adopts the orphaned job by its owner reference.
	orphaner, err = objects.Create(ctx, &controller.Object{Key: controller.ObjectKey{Namespace: "default", Name: "orphaner"}})
	must.NoError(t, err)
	owned, err := controller.AdoptJobs(ctx, backend, "default", orphaner, nil)
	must.NoError(t, err)
	must.Len(t, 1, owned)
	must.Eq(t, "kept-job", owned[0].ID)
	job, _, err := client.Jobs().Info("kept-job", nil)
	must.NoError(t, err)
	must.False(t, *job.Stop)
}
