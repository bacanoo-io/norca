// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/hashicorp/nomad/controller/fake"
	"github.com/shoenig/test/must"
)

func putNativeWidget(b *fake.Backend, ns, name string, spec map[string]any) *api.CustomObject {
	return b.PutObject(&api.CustomObject{
		Group: widgetKind.Group, Kind: widgetKind.Kind, Namespace: ns, Name: name, Spec: spec,
	})
}

func startNativeInformer(t *testing.T, b *fake.Backend, opts controller.InformerOptions) (*controller.Informer, *recorder) {
	t.Helper()
	opts.Logger = testLogger(t)
	inf, err := controller.NewNativeInformer(widgetKind, b, b, opts)
	must.NoError(t, err)
	rec := &recorder{}
	inf.AddHandler(rec.handler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		inf.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	must.True(t, inf.WaitForSync(ctx))
	return inf, rec
}

func TestNativeInformer_ListAndWatch(t *testing.T) {
	b := fake.New()
	putNativeWidget(b, "default", "a", map[string]any{"size": 1})
	putNativeWidget(b, "other", "x", nil) // other namespace
	b.PutObject(&api.CustomObject{Group: "example.com", Kind: "Gadget", Namespace: "default", Name: "g"})

	inf, rec := startNativeInformer(t, b, controller.InformerOptions{})
	must.Eq(t, []string{"add a"}, rec.get())

	putNativeWidget(b, "default", "b.v2", map[string]any{"size": 2})
	waitFor(t, "add", func() bool { return len(rec.get()) == 2 })
	putNativeWidget(b, "default", "a", map[string]any{"size": 3})
	waitFor(t, "update", func() bool { return len(rec.get()) == 3 })
	_, err := b.UpdateObjectStatus(context.Background(), widgetKind, "default", "a", map[string]any{"size": 3}, 0)
	must.NoError(t, err)
	waitFor(t, "status update", func() bool { return len(rec.get()) == 4 })
	b.RemoveObject(widgetKind, "default", "b.v2")
	waitFor(t, "delete", func() bool { return len(rec.get()) == 5 })
	must.Eq(t, []string{"add a", "add b.v2", "update a", "update a", "delete b.v2"}, rec.get())

	// Events carry the objects: nothing is read after the listing.
	must.Eq(t, 0, b.Calls("ReadObject"))
	must.Eq(t, 1, b.Calls("ListObjects"))

	obj, ok := inf.Get(controller.ObjectKey{Namespace: "default", Name: "a"})
	must.True(t, ok)
	must.Eq(t, `{"size":3}`, string(obj.Spec))
	must.Eq(t, `{"size":3}`, string(obj.Status))
	must.Eq(t, uint64(2), obj.Generation)
	must.NotEq(t, "", obj.ID)
}

func TestNativeInformer_ResyncOnGone(t *testing.T) {
	b := fake.New()
	putNativeWidget(b, "default", "a", nil)
	putNativeWidget(b, "default", "b", nil)
	metrics := controller.NewTextMetrics()
	inf, rec := startNativeInformer(t, b, controller.InformerOptions{Metrics: metrics, Name: "w"})
	must.Len(t, 2, inf.List())

	b.FailNext("Stream", errors.New("connection refused"))
	b.DropStreams()
	putNativeWidget(b, "default", "c", nil)
	b.RemoveObject(widgetKind, "default", "a")
	putNativeWidget(b, "default", "b", map[string]any{"changed": true})
	b.TruncateEvents(b.Index() + 1)

	waitFor(t, "resync", func() bool { return len(inf.List()) == 2 && len(rec.get()) == 5 })
	must.SliceContainsAll(t, []string{"add a", "add b", "add c", "delete a", "update b"}, rec.get())
	must.Eq(t, 1, metrics.Value("norca_controller_resync_total", "controller", "w", "source", "objects", "reason", "gone"))

	putNativeWidget(b, "default", "d", nil)
	waitFor(t, "event after resync", func() bool { return len(inf.List()) == 3 })
}

func TestNativeInformer_PollFallback(t *testing.T) {
	b := fake.New()
	b.SetStreamForbidden(true)
	putNativeWidget(b, "default", "a", nil)
	inf, rec := startNativeInformer(t, b, controller.InformerOptions{})
	must.Len(t, 1, inf.List())

	putNativeWidget(b, "default", "b", nil)
	putNativeWidget(b, "default", "a", map[string]any{"v": 2})
	waitFor(t, "polled changes", func() bool { return len(rec.get()) == 3 })
	b.RemoveObject(widgetKind, "default", "b")
	waitFor(t, "polled delete", func() bool { return len(inf.List()) == 1 })
}

func nativeWidgetStatus(t *testing.T, b *fake.Backend, name string) widgetStatus {
	co := b.GetObject(widgetKind, "default", name)
	must.NotNil(t, co)
	obj, err := controller.ObjectFromCustomObject(widgetKind, co)
	must.NoError(t, err)
	var s widgetStatus
	must.NoError(t, obj.DecodeStatus(&s))
	return s
}

func TestNativeController_ReconcileLifecycle(t *testing.T) {
	b := fake.New()
	putNativeWidget(b, "default", "a", map[string]any{"size": 1})
	c, r := startController(t, b, controller.Options{Storage: controller.StorageNative})
	must.Eq(t, controller.StorageNative, c.Objects().Storage())

	waitFor(t, "job created", func() bool { return jobCount(b, "widget-a") == 1 })
	waitFor(t, "status written", func() bool { return nativeWidgetStatus(t, b, "a").Size == 1 })
	st := nativeWidgetStatus(t, b, "a")
	must.True(t, st.IsTrue(controller.ConditionReady))
	must.Eq(t, uint64(1), st.ObservedGeneration)
	must.Eq(t, "", st.ObservedSpecHash)

	// The status is written on the status endpoint, never as a spec write
	// or a Variable.
	must.Positive(t, b.Calls("UpdateObjectStatus"))
	must.Eq(t, 0, b.Calls("ApplyObject"))
	must.Eq(t, 0, b.Calls("WriteVariable"))

	// Status writes do not trigger reconciles in a loop.
	time.Sleep(300 * time.Millisecond)
	calls := r.count("a")
	time.Sleep(300 * time.Millisecond)
	must.Eq(t, calls, r.count("a"))

	// A spec change increases the generation; the status observes it.
	putNativeWidget(b, "default", "a", map[string]any{"size": 3})
	waitFor(t, "job updated", func() bool { return jobCount(b, "widget-a") == 3 })
	waitFor(t, "status updated", func() bool { return nativeWidgetStatus(t, b, "a").ObservedGeneration == 2 })
	obj, ok := c.Get(controller.ObjectKey{Namespace: "default", Name: "a"})
	must.True(t, ok)
	waitFor(t, "cache updated", func() bool {
		obj, _ = c.Get(controller.ObjectKey{Namespace: "default", Name: "a"})
		return obj.ObservedGeneration() == 2
	})
	var cur widgetStatus
	must.NoError(t, obj.DecodeStatus(&cur))
	must.True(t, cur.Observed(obj))

	// Deleting the object deletes the owned job.
	b.RemoveObject(widgetKind, "default", "a")
	waitFor(t, "job deleted", func() bool { return b.GetJob("default", "widget-a") == nil })
}

func TestNativeObjects_Concurrency(t *testing.T) {
	b := fake.New()
	objects, err := controller.NewNativeObjects(widgetKind, b)
	must.NoError(t, err)
	ctx := context.Background()

	key := controller.ObjectKey{Namespace: "default", Name: "c.1"}
	created, err := objects.Create(ctx, &controller.Object{Key: key, Spec: []byte(`{"size":1}`), Labels: map[string]string{"team": "a"}})
	must.NoError(t, err)
	must.Eq(t, uint64(1), created.Generation)
	_, err = objects.Create(ctx, created)
	must.ErrorIs(t, err, controller.ErrConflict)

	// A concurrent spec change: the stale update conflicts, the status
	// update re-reads and keeps the new spec and generation.
	newer := created.Copy()
	newer.Spec = []byte(`{"size":9}`)
	updated, err := objects.Update(ctx, newer)
	must.NoError(t, err)
	must.Eq(t, uint64(2), updated.Generation)
	_, err = objects.Update(ctx, created)
	must.ErrorIs(t, err, controller.ErrConflict)

	out, err := objects.UpdateStatus(ctx, created, widgetStatus{Size: 1})
	must.NoError(t, err)
	must.Eq(t, `{"size":9}`, string(out.Spec))
	must.Eq(t, uint64(2), out.Generation)
	must.Eq(t, 2, b.Calls("UpdateObjectStatus"))

	// An unchanged status is not written again.
	_, err = objects.UpdateStatus(ctx, out, widgetStatus{Size: 1})
	must.NoError(t, err)
	must.Eq(t, 2, b.Calls("UpdateObjectStatus"))

	// A label-only update keeps the generation and the status.
	labeled := out.Copy()
	labeled.Labels = map[string]string{"team": "b"}
	labeled, err = objects.Update(ctx, labeled)
	must.NoError(t, err)
	must.Eq(t, uint64(2), labeled.Generation)
	must.Eq(t, string(out.Status), string(labeled.Status))

	list, _, err := objects.List(ctx, "*")
	must.NoError(t, err)
	must.Len(t, 1, list)

	must.ErrorIs(t, objects.Delete(ctx, key, created.ModifyIndex), controller.ErrConflict)
	must.NoError(t, objects.Delete(ctx, key, labeled.ModifyIndex))
	_, err = objects.Get(ctx, key)
	must.ErrorIs(t, err, controller.ErrNotFound)
	must.NoError(t, objects.Delete(ctx, key, 0))

	// Native names may contain dots, but not slashes.
	_, err = objects.Get(ctx, controller.ObjectKey{Namespace: "default", Name: "a/b"})
	must.Error(t, err)
}

func TestObjectFromCustomObject(t *testing.T) {
	co := &api.CustomObject{
		ID: "id", Group: "example.com", Kind: "Widget", Namespace: "prod", Name: "w.1",
		Labels: map[string]string{"team": "x"}, ManagedBy: "gitops", Generation: 3,
		Spec:        map[string]any{"size": float64(2)},
		Status:      map[string]any{"observedGeneration": float64(3)},
		CreateIndex: 10, ModifyIndex: 12,
	}
	obj, err := controller.ObjectFromCustomObject(widgetKind, co)
	must.NoError(t, err)
	must.Eq(t, controller.ObjectKey{Namespace: "prod", Name: "w.1"}, obj.Key)
	must.Eq(t, `{"size":2}`, string(obj.Spec))
	must.Eq(t, uint64(3), obj.ObservedGeneration())
	must.Eq(t, "gitops", obj.ManagedBy)

	back, err := obj.ToCustomObject()
	must.NoError(t, err)
	must.Eq(t, co.Spec, back.Spec)
	must.Eq(t, co.Status, back.Status)
	must.Eq(t, co.Labels, back.Labels)
	must.Eq(t, co.Generation, back.Generation)

	_, err = controller.ObjectFromCustomObject(controller.Kind{Group: "example.com", Kind: "Gadget"}, co)
	must.Error(t, err)

	// Observe and Observed use the generation of native objects and the
	// spec hash of Variables objects.
	var s controller.CommonStatus
	s.Observe(obj)
	must.Eq(t, uint64(3), s.ObservedGeneration)
	must.True(t, s.Observed(obj))
	obj.Generation = 4
	must.False(t, s.Observed(obj))

	vobj := &controller.Object{Kind: widgetKind, Spec: json.RawMessage(`{"size":1}`)}
	var vs controller.CommonStatus
	vs.Observe(vobj)
	must.Eq(t, vobj.SpecHash(), vs.ObservedSpecHash)
	must.True(t, vs.Observed(vobj))

	for in, want := range map[string]controller.Storage{"": controller.StorageVariables, "variables": controller.StorageVariables, "native": controller.StorageNative} {
		got, err := controller.ParseStorage(in)
		must.NoError(t, err)
		must.Eq(t, want, got)
	}
	_, err = controller.ParseStorage("etcd")
	must.Error(t, err)
}
