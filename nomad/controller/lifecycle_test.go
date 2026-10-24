// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/hashicorp/nomad/controller/fake"
	"github.com/shoenig/test/must"
)

func TestLifecycle_Finalizers(t *testing.T) {
	b := fake.New()
	objects, err := controller.NewNativeObjects(widgetKind, b)
	must.NoError(t, err)
	ctx := context.Background()

	obj, err := objects.Create(ctx, &controller.Object{Key: controller.ObjectKey{Namespace: "default", Name: "a"}})
	must.NoError(t, err)
	obj, err = objects.AddFinalizer(ctx, obj, "example.com/x")
	must.NoError(t, err)
	must.Eq(t, []string{"example.com/x"}, obj.Finalizers)

	// A spec update keeps the finalizer.
	obj.Spec = []byte(`{"size":2}`)
	obj, err = objects.Update(ctx, obj)
	must.NoError(t, err)
	must.True(t, obj.HasFinalizer("example.com/x"))

	// The deletion marks the object; the default predicate reconciles it.
	b.RemoveObject(widgetKind, "default", "a")
	marked, err := objects.Get(ctx, obj.Key)
	must.NoError(t, err)
	must.True(t, marked.Deleting())
	must.True(t, controller.SpecChanged(obj, marked))

	_, err = objects.RemoveFinalizer(ctx, marked, "example.com/x")
	must.NoError(t, err)
	_, err = objects.Get(ctx, obj.Key)
	must.ErrorIs(t, err, controller.ErrNotFound)

	// Variables storage has no finalizers.
	vars, err := controller.NewObjects(widgetKind, b)
	must.NoError(t, err)
	_, err = vars.AddFinalizer(ctx, obj, "example.com/y")
	must.ErrorIs(t, err, controller.ErrUnsupported)
	must.NoError(t, vars.RecordEvent(ctx, obj.Key, controller.EventTypeNormal, "Ignored", ""))
}

func TestLifecycle_EventRecorder(t *testing.T) {
	b := fake.New()
	objects, err := controller.NewNativeObjects(widgetKind, b)
	must.NoError(t, err)
	ctx := context.Background()
	key := controller.ObjectKey{Namespace: "default", Name: "a"}
	_, err = objects.Create(ctx, &controller.Object{Key: key})
	must.NoError(t, err)

	rec := controller.NewEventRecorder(objects, time.Hour)
	rec.Event(ctx, key, controller.EventTypeNormal, "Reconciled", "ok")
	rec.Event(ctx, key, controller.EventTypeNormal, "Reconciled", "ok")
	must.Eq(t, 1, b.Calls("RecordObjectEvents"))
	rec.Event(ctx, key, controller.EventTypeWarning, "Failed", "boom")
	rec.Event(ctx, key, controller.EventTypeNormal, "Reconciled", "ok")
	must.Eq(t, 3, b.Calls("RecordObjectEvents"))

	events := b.ObjectEvents(widgetKind, "default", "a")
	must.Len(t, 3, events)
	must.Eq(t, "Failed", events[1].Reason)

	// Missing objects are not an error.
	rec.Event(ctx, controller.ObjectKey{Namespace: "default", Name: "missing"}, controller.EventTypeNormal, "X", "")
}

func TestLifecycle_OwnersAndAdoption(t *testing.T) {
	b := fake.New()
	ctx := context.Background()
	owner := &controller.Object{Kind: widgetKind, Key: controller.ObjectKey{Namespace: "default", Name: "o"}, ID: "id-o"}

	job := api.NewServiceJob("j", "j", "global", 50)
	controller.SetOwner(job, owner)
	controller.SetManagedBy(job, "workload:default/ctrl")
	must.Eq(t, widgetKind.String(), job.Meta[controller.OwnerKindMetaKey])
	must.Eq(t, "workload:default/ctrl", job.Meta[api.JobMetaManagedBy])
	must.True(t, controller.IsOwnedBy(job.Meta, owner))
	key, ok := controller.OwnerFromMeta(widgetKind, job.Meta)
	must.True(t, ok)
	must.Eq(t, owner.Key, key)

	// The owner kind disambiguates kinds whose groups map to the same path.
	other := controller.Kind{Group: "example-com", Kind: widgetKind.Kind}
	_, ok = controller.OwnerFromMeta(other, job.Meta)
	must.False(t, ok)

	// Objects owned by objects.
	child := &controller.Object{Kind: widgetKind, Key: controller.ObjectKey{Namespace: "default", Name: "c"}}
	controller.SetObjectOwner(child, owner)
	controller.SetObjectOwner(child, owner)
	must.Len(t, 1, child.Owners)
	must.True(t, controller.IsObjectOwnedBy(child, owner))

	// Adoption: owned jobs are found, unowned jobs matching the selector
	// get the owner reference, others are left alone.
	b.PutJob(job)
	legacy := api.NewServiceJob("legacy", "legacy", "global", 50)
	legacy.Meta = map[string]string{"app": "o"}
	b.PutJob(legacy)
	unrelated := api.NewServiceJob("unrelated", "unrelated", "global", 50)
	unrelated.Meta = map[string]string{"app": "x"}
	b.PutJob(unrelated)

	owned, err := controller.AdoptJobs(ctx, b, "default", owner, map[string]string{"app": "o"})
	must.NoError(t, err)
	must.Len(t, 2, owned)
	must.True(t, controller.IsOwnedBy(b.GetJob("default", "legacy").Meta, owner))
	must.False(t, controller.IsOwnedBy(b.GetJob("default", "unrelated").Meta, owner))
}

func TestLifecycle_ManagedByFromEnv(t *testing.T) {
	t.Setenv("NOMAD_NAMESPACE", "")
	t.Setenv("NOMAD_JOB_ID", "")
	must.Eq(t, "", controller.ManagedByFromEnv())
	t.Setenv("NOMAD_NAMESPACE", "ops")
	t.Setenv("NOMAD_JOB_ID", "lb-controller")
	must.Eq(t, "workload:ops/lb-controller", controller.ManagedByFromEnv())

	job := api.NewServiceJob("j", "j", "global", 50)
	controller.SetManagedBy(job, "")
	must.MapNotContainsKey(t, job.Meta, api.JobMetaManagedBy)
}

func TestLifecycle_PreserveScaledCounts(t *testing.T) {
	group := func(name string, count int, scaling *api.ScalingPolicy) *api.TaskGroup {
		tg := api.NewTaskGroup(name, count)
		tg.Scaling = scaling
		return tg
	}
	enabled := true
	existing := api.NewServiceJob("j", "j", "global", 50)
	existing.TaskGroups = []*api.TaskGroup{group("own", 5, nil), group("auto", 7, nil), group("object", 3, nil)}

	desired := api.NewServiceJob("j", "j", "global", 50)
	desired.TaskGroups = []*api.TaskGroup{
		group("own", 2, nil),
		group("auto", 2, &api.ScalingPolicy{Enabled: &enabled, Min: pointer(int64(1)), Max: pointer(int64(10))}),
		group("object", 2, &api.ScalingPolicy{Policy: map[string]any{
			"target": []any{map[string]any{"custom-object": []any{map[string]any{"kind": "g/K", "name": "n"}}}},
		}}),
	}
	controller.PreserveScaledCounts(desired, existing)
	must.Eq(t, 2, *desired.TaskGroups[0].Count) // no scaling block: the controller decides
	must.Eq(t, 7, *desired.TaskGroups[1].Count) // scaled by the autoscaler
	must.Eq(t, 2, *desired.TaskGroups[2].Count) // the autoscaler scales the object
}

func pointer[T any](v T) *T { return &v }
