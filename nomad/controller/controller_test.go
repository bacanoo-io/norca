// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/hashicorp/nomad/controller/fake"
	"github.com/shoenig/test/must"
)

type widgetSpec struct {
	Size int `json:"size"`
}

type widgetStatus struct {
	controller.CommonStatus
	Size int `json:"size"`
}

// widgetReconciler keeps one job per widget, named widget-<name>, with the
// size as group count, and reports the size in the status.
type widgetReconciler struct {
	c *controller.Controller

	// decorate, if set, completes the job (e.g. with a task for a real
	// server).
	decorate func(*api.Job)

	mu    sync.Mutex
	calls map[string]int
	fail  map[string]int // remaining failures per object
}

func (r *widgetReconciler) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func (r *widgetReconciler) Reconcile(ctx context.Context, key controller.ObjectKey) (time.Duration, error) {
	r.mu.Lock()
	r.calls[key.Name]++
	if r.fail[key.Name] > 0 {
		r.fail[key.Name]--
		r.mu.Unlock()
		return 0, errors.New("injected failure")
	}
	r.mu.Unlock()

	b := r.c.Backend()
	jobID := "widget-" + key.Name
	obj, ok := r.c.Get(key)
	if !ok {
		return 0, b.DeregisterJob(ctx, key.Namespace, jobID, true)
	}
	var spec widgetSpec
	if err := obj.DecodeSpec(&spec); err != nil {
		return 0, err
	}
	job := api.NewServiceJob(jobID, jobID, "global", 50)
	job.Namespace = &key.Namespace
	job.AddTaskGroup(api.NewTaskGroup("g", spec.Size))
	controller.SetOwner(job, obj)
	if r.decorate != nil {
		r.decorate(job)
	}

	cur, err := b.ReadJob(ctx, key.Namespace, jobID)
	switch {
	case errors.Is(err, controller.ErrNotFound):
		err = b.RegisterJob(ctx, job)
	case err != nil:
	case *cur.TaskGroups[0].Count != spec.Size || (cur.Stop != nil && *cur.Stop):
		err = b.RegisterJob(ctx, job)
	}
	if err != nil {
		return 0, err
	}

	var status widgetStatus
	_ = obj.DecodeStatus(&status)
	status.Observe(obj)
	status.Size = spec.Size
	status.SetCondition(controller.Condition{Type: controller.ConditionReady, Status: controller.ConditionTrue}, time.Now())
	_, err = r.c.Objects().UpdateStatus(ctx, obj, status)
	return 0, err
}

func startController(t *testing.T, b *fake.Backend, opts controller.Options) (*controller.Controller, *widgetReconciler) {
	t.Helper()
	r := &widgetReconciler{calls: map[string]int{}, fail: map[string]int{}}
	if opts.Name == "" {
		opts.Name = "widgets"
	}
	opts.Kind = widgetKind
	opts.OwnsJobs = true
	opts.Logger = testLogger(t)
	if opts.RateLimiter == nil {
		opts.RateLimiter = controller.NewExponentialBackoff[controller.ObjectKey](10*time.Millisecond, 100*time.Millisecond)
	}
	c, err := controller.New(b, r, opts)
	must.NoError(t, err)
	r.c = c
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return c, r
}

func jobCount(b *fake.Backend, id string) int {
	j := b.GetJob("default", id)
	if j == nil || (j.Stop != nil && *j.Stop) {
		return -1
	}
	return *j.TaskGroups[0].Count
}

func widgetStatusOf(t *testing.T, b *fake.Backend, name string) widgetStatus {
	v := b.GetVariable("default", widgetKind.Path(name))
	must.NotNil(t, v)
	obj, err := controller.ObjectFromVariable(widgetKind, v)
	must.NoError(t, err)
	var s widgetStatus
	must.NoError(t, obj.DecodeStatus(&s))
	return s
}

func TestController_ReconcileLifecycle(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "a", `{"size":1}`)
	_, r := startController(t, b, controller.Options{})

	waitFor(t, "job created", func() bool { return jobCount(b, "widget-a") == 1 })
	waitFor(t, "status written", func() bool { return widgetStatusOf(t, b, "a").Size == 1 })
	st := widgetStatusOf(t, b, "a")
	must.True(t, st.IsTrue(controller.ConditionReady))

	// The status write does not trigger reconciles in a loop.
	time.Sleep(300 * time.Millisecond)
	calls := r.count("a")
	time.Sleep(300 * time.Millisecond)
	must.Eq(t, calls, r.count("a"))

	// A spec change updates the job; the status records the new spec.
	putWidget(b, "default", "a", `{"size":3}`)
	waitFor(t, "job updated", func() bool { return jobCount(b, "widget-a") == 3 })
	waitFor(t, "status updated", func() bool { return widgetStatusOf(t, b, "a").Size == 3 })
	obj, err := controller.ObjectFromVariable(widgetKind, b.GetVariable("default", widgetKind.Path("a")))
	must.NoError(t, err)
	must.Eq(t, obj.SpecHash(), widgetStatusOf(t, b, "a").ObservedSpecHash)

	// Deleting the object deletes the owned job.
	b.RemoveVariable("default", widgetKind.Path("a"))
	waitFor(t, "job deleted", func() bool { return b.GetJob("default", "widget-a") == nil })
}

func TestController_OwnedJobsRepair(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "a", `{"size":2}`)
	_, r := startController(t, b, controller.Options{})
	waitFor(t, "job created", func() bool { return jobCount(b, "widget-a") == 2 })
	waitFor(t, "first reconcile done", func() bool { return r.count("a") >= 1 })

	// Someone scales the owned job: the change maps back to the owner.
	j := b.GetJob("default", "widget-a")
	*j.TaskGroups[0].Count = 5
	b.PutJob(j)
	waitFor(t, "job repaired", func() bool { return jobCount(b, "widget-a") == 2 })

	// Someone purges the owned job: the Job event of the purge (on older
	// servers only the evaluation, through the owned jobs index) maps back
	// to the owner.
	b.RemoveJob("default", "widget-a", true)
	waitFor(t, "job recreated", func() bool { return jobCount(b, "widget-a") == 2 })

	// A job that is not owned does not trigger reconciles.
	before := r.count("a")
	other := api.NewServiceJob("other", "other", "global", 50)
	other.AddTaskGroup(api.NewTaskGroup("g", 1))
	b.PutJob(other)
	time.Sleep(200 * time.Millisecond)
	must.Eq(t, before, r.count("a"))
}

func TestController_RetryWithBackoff(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "a", `{"size":1}`)
	metrics := controller.NewTextMetrics()
	r := &widgetReconciler{calls: map[string]int{}, fail: map[string]int{"a": 3}}
	c, err := controller.New(b, r, controller.Options{
		Name:        "widgets",
		Kind:        widgetKind,
		Logger:      testLogger(t),
		Metrics:     metrics,
		RateLimiter: controller.NewExponentialBackoff[controller.ObjectKey](10*time.Millisecond, 50*time.Millisecond),
	})
	must.NoError(t, err)
	r.c = c
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	waitFor(t, "job created after retries", func() bool { return jobCount(b, "widget-a") == 1 })
	must.Eq(t, 4, r.count("a"))
	must.Eq(t, 3, metrics.Value("norca_controller_reconcile_total", "controller", "widgets", "result", controller.ResultError))
}

func TestController_StatusConflictRetry(t *testing.T) {
	b := fake.New()
	v := putWidget(b, "default", "a", `{"size":1}`)
	objects, err := controller.NewObjects(widgetKind, b)
	must.NoError(t, err)
	obj, err := controller.ObjectFromVariable(widgetKind, v)
	must.NoError(t, err)

	// A user changes the spec after the controller read the object.
	putWidget(b, "default", "a", `{"size":7}`)

	out, err := objects.UpdateStatus(context.Background(), obj, widgetStatus{Size: 1})
	must.NoError(t, err)
	must.Eq(t, `{"size":7}`, string(out.Spec)) // the user's spec is kept
	must.Eq(t, 1, widgetStatusOf(t, b, "a").Size)

	// An unchanged status is not written again.
	writes := b.Calls("WriteVariable")
	_, err = objects.UpdateStatus(context.Background(), out, widgetStatus{Size: 1})
	must.NoError(t, err)
	must.Eq(t, writes, b.Calls("WriteVariable"))

	// Create fails if the object exists; Update with a stale index conflicts.
	_, err = objects.Create(context.Background(), obj)
	must.ErrorIs(t, err, controller.ErrConflict)
	_, err = objects.Update(context.Background(), obj)
	must.ErrorIs(t, err, controller.ErrConflict)
}

func TestController_LeaderElection(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "a", `{"size":1}`)
	le := leaseConfig("")
	le.Path = ""
	c1, r1 := startController(t, b, controller.Options{LeaderElection: &le, Name: "widgets"})
	waitFor(t, "c1 leads", c1.IsLeader)
	waitFor(t, "job created", func() bool { return jobCount(b, "widget-a") == 1 })

	le2 := le
	le2.Identity = "second"
	c2, r2 := startController(t, b, controller.Options{LeaderElection: &le2, Name: "widgets"})

	putWidget(b, "default", "a", `{"size":2}`)
	waitFor(t, "job updated", func() bool { return jobCount(b, "widget-a") == 2 })
	must.False(t, c2.IsLeader())
	must.Eq(t, 0, r2.count("a"))
	must.Greater(t, 0, r1.count("a"))
	must.NotNil(t, b.GetVariable("default", "controller-lease/widgets"))
}

func TestController_EventSource(t *testing.T) {
	b := fake.New()
	putWidget(b, "default", "a", `{"size":1}`)
	src := &controller.EventSource{
		SourceName: "services",
		Topics:     map[api.Topic][]string{api.TopicService: {"*"}},
		Map: func(ev *api.Event, sc *controller.SourceContext) []controller.ObjectKey {
			var keys []controller.ObjectKey
			for _, o := range sc.Objects() {
				if o.Key.Name == ev.Key {
					keys = append(keys, o.Key)
				}
			}
			return keys
		},
	}
	_, r := startController(t, b, controller.Options{Sources: []controller.Source{src}})
	waitFor(t, "first reconcile", func() bool { return r.count("a") >= 1 })
	time.Sleep(100 * time.Millisecond)
	before := r.count("a")

	b.Publish(api.Event{Topic: api.TopicService, Type: "ServiceRegistration", Key: "a", Namespace: "default"})
	waitFor(t, "reconciled from event", func() bool { return r.count("a") > before })
}
