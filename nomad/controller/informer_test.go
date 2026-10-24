// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/hashicorp/nomad/controller/fake"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

var widgetKind = controller.Kind{Group: "example.com", Kind: "Widget"}

func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

func putWidget(b *fake.Backend, ns, name, spec string) *api.Variable {
	return b.PutVariable(&api.Variable{
		Namespace: ns,
		Path:      widgetKind.Path(name),
		Items:     api.VariableItems{"spec": spec},
	})
}

// recorder records informer notifications.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) handler() controller.EventHandler {
	add := func(s string) {
		r.mu.Lock()
		r.events = append(r.events, s)
		r.mu.Unlock()
	}
	return controller.EventHandler{
		OnAdd:    func(o *controller.Object) { add("add " + o.Key.Name) },
		OnUpdate: func(_, o *controller.Object) { add("update " + o.Key.Name) },
		OnDelete: func(o *controller.Object) { add("delete " + o.Key.Name) },
	}
}

func (r *recorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func waitFor(t *testing.T, desc string, fn func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(fn),
		wait.Timeout(5*time.Second),
		wait.Gap(10*time.Millisecond),
	), must.Sprint(desc))
}

func startInformer(t *testing.T, b *fake.Backend, opts controller.InformerOptions) (*controller.Informer, *recorder) {
	t.Helper()
	opts.Logger = testLogger(t)
	inf, err := controller.NewInformer(widgetKind, b, b, opts)
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

func TestInformer_ListAndWatch(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "a", `{"size":1}`)
	putWidget(b, "other", "x", `{}`)                                                                       // other namespace
	b.PutVariable(&api.Variable{Path: "crd/example-com/Gadget/g", Items: api.VariableItems{"spec": "{}"}}) // other kind
	b.PutVariable(&api.Variable{Path: widgetKind.Path("a") + "/nested", Items: api.VariableItems{"spec": "{}"}})

	inf, rec := startInformer(t, b, controller.InformerOptions{})
	must.Eq(t, []string{"add a"}, rec.get())

	// Events carry only metadata and the informer reads the current object,
	// so changes are applied step by step here.
	putWidget(b, "default", "b", `{"size":2}`)
	waitFor(t, "add", func() bool { return len(rec.get()) == 2 })
	putWidget(b, "default", "a", `{"size":3}`)
	waitFor(t, "update", func() bool { return len(rec.get()) == 3 })
	b.RemoveVariable("default", widgetKind.Path("b"))
	waitFor(t, "delete", func() bool { return len(rec.get()) == 4 })
	must.Eq(t, []string{"add a", "add b", "update a", "delete b"}, rec.get())

	obj, ok := inf.Get(controller.ObjectKey{Namespace: "default", Name: "a"})
	must.True(t, ok)
	must.Eq(t, `{"size":3}`, string(obj.Spec))
	must.Len(t, 1, inf.List())
}

func TestInformer_ResyncOnGone(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "a", `{}`)
	putWidget(b, "default", "b", `{}`)
	metrics := controller.NewTextMetrics()
	inf, rec := startInformer(t, b, controller.InformerOptions{Metrics: metrics, Name: "w"})
	must.Len(t, 2, inf.List())

	// Changes happen while the events are dropped from the buffer: the
	// informer gets 410 Gone on reconnect and lists again.
	b.FailNext("Stream", errors.New("connection refused"))
	b.DropStreams()
	putWidget(b, "default", "c", `{}`)
	b.RemoveVariable("default", widgetKind.Path("a"))
	putWidget(b, "default", "b", `{"changed":true}`)
	b.TruncateEvents(b.Index() + 1)

	waitFor(t, "resync", func() bool { return len(inf.List()) == 2 && len(rec.get()) == 5 })
	must.SliceContainsAll(t, []string{"add a", "add b", "add c", "delete a", "update b"}, rec.get())
	must.Eq(t, 1, metrics.Value("norca_controller_resync_total", "controller", "w", "source", "objects", "reason", "gone"))

	// Streaming continues after the resync.
	putWidget(b, "default", "d", `{}`)
	waitFor(t, "event after resync", func() bool { return len(inf.List()) == 3 })
}

func TestInformer_ResumeWithoutLoss(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "first", `{}`)
	inf, rec := startInformer(t, b, controller.InformerOptions{})

	// A disconnection without buffer loss resumes exactly: no relisting.
	b.FailNext("Stream", errors.New("connection refused"))
	b.DropStreams()
	for i := range 5 {
		putWidget(b, "default", fmt.Sprintf("w%d", i), `{}`)
	}
	waitFor(t, "all objects", func() bool { return len(inf.List()) == 6 })
	must.Len(t, 6, rec.get())
	must.Eq(t, 1, b.Calls("ListVariables"))
}

func TestInformer_PollFallback(t *testing.T) {
	b := fake.New()
	b.SetStreamForbidden(true)
	putWidget(b, "default", "a", `{}`)
	inf, rec := startInformer(t, b, controller.InformerOptions{})
	must.Len(t, 1, inf.List())

	putWidget(b, "default", "b", `{}`)
	putWidget(b, "default", "a", `{"v":2}`)
	waitFor(t, "polled changes", func() bool { return len(rec.get()) == 3 })
	b.RemoveVariable("default", widgetKind.Path("b"))
	waitFor(t, "polled delete", func() bool { return len(inf.List()) == 1 })
	must.SliceContainsAll(t, []string{"add a", "add b", "update a", "delete b"}, rec.get())
}

func TestInformer_ReadErrorRelists(t *testing.T) {
	b := fake.New()
	inf, _ := startInformer(t, b, controller.InformerOptions{})

	b.FailNext("ReadVariable", errors.New("temporary failure"))
	putWidget(b, "default", "a", `{}`)
	waitFor(t, "object after relist", func() bool { return len(inf.List()) == 1 })
	must.Eq(t, 2, b.Calls("ListVariables"))
}
