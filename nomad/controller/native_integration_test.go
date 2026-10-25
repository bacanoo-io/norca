// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/shoenig/test/must"
)

const widgetSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "properties": {"size": {"type": "integer", "minimum": 0, "default": 1}}
    },
    "status": {
      "type": "object",
      "properties": {"observedGeneration": {"type": "integer"}, "size": {"type": "integer"}}
    }
  }
}`

func registerWidgetKind(t *testing.T, client *api.Client) {
	_, _, err := client.ObjectKinds().Register(&api.ObjectKind{
		Group:      widgetKind.Group,
		Kind:       widgetKind.Kind,
		Namespaced: true,
		Schema:     widgetSchema,
	}, nil, nil)
	must.NoError(t, err)
}

func applyWidgetAPI(t *testing.T, client *api.Client, name string, spec map[string]any) *api.CustomObject {
	out, _, err := client.Objects().Apply(&api.CustomObject{
		Group: widgetKind.Group, Kind: widgetKind.Kind, Namespace: "default", Name: name, Spec: spec,
	}, nil, nil)
	must.NoError(t, err)
	return out
}

func readNativeWidget(t *testing.T, client *api.Client, name string) (*controller.Object, bool) {
	co, _, err := client.Objects().Get(widgetKind.Group, widgetKind.Kind, name, nil)
	if err != nil {
		return nil, false
	}
	obj, err := controller.ObjectFromCustomObject(widgetKind, co)
	must.NoError(t, err)
	return obj, true
}

func TestIntegration_NativeController(t *testing.T) {
	client := startAgent(t, 8)
	registerWidgetKind(t, client)
	backend := newGated(controller.NewBackend(client))
	metrics := controller.NewTextMetrics()

	r := &widgetReconciler{calls: map[string]int{}, fail: map[string]int{}, decorate: pendingJob}
	c, err := controller.New(backend, r, controller.Options{
		Name:        "widgets",
		Kind:        widgetKind,
		Storage:     controller.StorageNative,
		OwnsJobs:    true,
		Metrics:     metrics,
		Logger:      testLogger(t),
		RateLimiter: controller.NewExponentialBackoff[controller.ObjectKey](50*time.Millisecond, time.Second),
	})
	must.NoError(t, err)
	r.c = c
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// The server fills in the default size.
	created := applyWidgetAPI(t, client, "a", map[string]any{})
	must.Eq(t, uint64(1), created.Generation)
	waitForLong(t, "job created", func() bool { return jobCountAPI(client, "widget-a") == 1 })
	waitForLong(t, "status written", func() bool {
		obj, ok := readNativeWidget(t, client, "a")
		var st widgetStatus
		return ok && obj.DecodeStatus(&st) == nil && st.Size == 1 && st.ObservedGeneration == 1
	})
	obj, _ := readNativeWidget(t, client, "a")
	must.Eq(t, uint64(1), obj.Generation) // status writes keep the generation
	job, _, err := client.Jobs().Info("widget-a", nil)
	must.NoError(t, err)
	must.Eq(t, "default/crd/example-com/Widget/a", job.Meta[controller.OwnerMetaKey])

	// A spec change through the API: new generation, job updated, observed.
	applyWidgetAPI(t, client, "a", map[string]any{"size": 2})
	waitForLong(t, "job updated", func() bool { return jobCountAPI(client, "widget-a") == 2 })
	waitForLong(t, "generation observed", func() bool {
		obj, ok := readNativeWidget(t, client, "a")
		return ok && obj.Generation == 2 && obj.ObservedGeneration() == 2
	})

	// Spec writes of the SDK keep the status.
	obj, _ = readNativeWidget(t, client, "a")
	obj.Spec = []byte(`{"size":3}`)
	updated, err := c.Objects().Update(context.Background(), obj)
	must.NoError(t, err)
	must.Eq(t, uint64(3), updated.Generation)
	must.Eq(t, uint64(2), updated.ObservedGeneration())
	waitForLong(t, "job updated again", func() bool { return jobCountAPI(client, "widget-a") == 3 })

	// The controller is cut off while more changes happen than the event
	// buffer holds: on reconnect the server answers 410 Gone and the
	// informer lists again, without losing a change.
	backend.cut()
	for i := range 12 {
		applyWidgetAPI(t, client, fmt.Sprintf("b%d", i), map[string]any{"size": 1})
	}
	_, _, err = client.Objects().Delete(widgetKind.Group, widgetKind.Kind, "b0", nil, nil)
	must.NoError(t, err)
	backend.restore()
	waitForLong(t, "resynchronized", func() bool { return len(c.Informer().List()) == 12 })
	must.Greater(t, 0, metrics.Value("norca_controller_resync_total",
		"controller", "widgets", "source", "objects", "reason", "gone"))
	waitForLong(t, "jobs of new objects", func() bool { return jobCountAPI(client, "widget-b11") == 1 })
	must.Eq(t, -1, jobCountAPI(client, "widget-b0"))

	// Deleting an object deletes its job.
	_, _, err = client.Objects().Delete(widgetKind.Group, widgetKind.Kind, "a", nil, nil)
	must.NoError(t, err)
	waitForLong(t, "job deleted", func() bool {
		_, _, err := client.Jobs().Info("widget-a", nil)
		return err != nil
	})
}

func TestIntegration_NativeObjects(t *testing.T) {
	client := startAgent(t, 100)
	registerWidgetKind(t, client)
	backend := controller.NewBackend(client)
	objects, err := controller.NewNativeObjects(widgetKind, backend)
	must.NoError(t, err)
	ctx := context.Background()

	key := controller.ObjectKey{Namespace: "default", Name: "c.1"}
	created, err := objects.Create(ctx, &controller.Object{Key: key, Spec: []byte(`{"size":1}`)})
	must.NoError(t, err)
	_, err = objects.Create(ctx, created)
	must.ErrorIs(t, err, controller.ErrConflict)

	// A concurrent spec change: the stale update gets 409, the status
	// update re-reads and keeps the new spec.
	newer := created.Copy()
	newer.Spec = []byte(`{"size":9}`)
	_, err = objects.Update(ctx, newer)
	must.NoError(t, err)
	_, err = objects.Update(ctx, created)
	must.ErrorIs(t, err, controller.ErrConflict)

	out, err := objects.UpdateStatus(ctx, created, widgetStatus{Size: 1})
	must.NoError(t, err)
	must.Eq(t, `{"size":9}`, string(out.Spec))
	must.Eq(t, uint64(2), out.Generation)

	// The schema is enforced by the server.
	bad := out.Copy()
	bad.Spec = []byte(`{"size":-1}`)
	_, err = objects.Update(ctx, bad)
	must.ErrorContains(t, err, "400")

	_, err = backend.UpdateObjectStatus(ctx, widgetKind, "default", "c.1", map[string]any{"size": 2}, created.ModifyIndex)
	must.ErrorIs(t, err, controller.ErrConflict)
	_, err = backend.ReadObject(ctx, widgetKind, "default", "missing")
	must.ErrorIs(t, err, controller.ErrNotFound)

	list, _, err := objects.List(ctx, "*")
	must.NoError(t, err)
	must.Len(t, 1, list)

	must.ErrorIs(t, objects.Delete(ctx, key, created.ModifyIndex), controller.ErrConflict)
	must.NoError(t, objects.Delete(ctx, key, 0))
	must.NoError(t, objects.Delete(ctx, key, 0)) // missing: not an error
}
