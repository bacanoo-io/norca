// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Reconciler brings the world in line with the desired state of one object.
//
// Reconcile is called with the key of an object whenever the object, a job
// it owns or another watched resource changed, and periodically. The object
// may no longer exist (Controller.Get returns false): then the reconciler
// cleans up what the object owned. Reconcile must be idempotent; it should
// read the current state instead of relying on what triggered it.
//
// A non-nil error retries the key with the rate limiter's backoff. A
// positive requeueAfter (with a nil error) reconciles the key again after
// that delay, for example to poll an external system.
type Reconciler interface {
	Reconcile(ctx context.Context, key ObjectKey) (requeueAfter time.Duration, err error)
}

// ReconcilerFunc adapts a function to the Reconciler interface.
type ReconcilerFunc func(ctx context.Context, key ObjectKey) (time.Duration, error)

func (f ReconcilerFunc) Reconcile(ctx context.Context, key ObjectKey) (time.Duration, error) {
	return f(ctx, key)
}

// Predicate decides whether an update of an object is reconciled.
type Predicate func(oldObj, newObj *Object) bool

// SpecChanged is the default predicate: it reconciles changes of the spec,
// labels or extra items, and objects whose status was removed. Status-only
// changes (typically the controller's own status writes) are skipped.
func SpecChanged(oldObj, newObj *Object) bool {
	return !specEqual(oldObj, newObj) || len(newObj.Status) == 0
}

// AnyChange reconciles every update of an object.
func AnyChange(_, _ *Object) bool { return true }

// Options configures a Controller.
type Options struct {
	// Name of the controller, used in logs, metrics and the default lease
	// path. Letters, digits, '-', '_' and '~' only.
	Name string

	// Kind of the objects the controller reconciles.
	Kind Kind

	// Namespace to watch; "*" for every namespace the token can access.
	// Defaults to "default".
	Namespace string

	// Workers is the number of concurrent reconciles (default 1). A key is
	// never reconciled by two workers at the same time.
	Workers int

	// RateLimiter for retries after errors; DefaultRateLimiter if nil.
	RateLimiter RateLimiter[ObjectKey]

	// ResyncPeriod reconciles every object and lists the objects again
	// periodically (default 10 minutes; negative disables it).
	ResyncPeriod time.Duration

	// WatchMode selects the event stream, blocking queries, or the event
	// stream with fallback to blocking queries (default).
	WatchMode WatchMode

	// OwnsJobs watches the jobs owned by the objects (OwnerMetaKey) and
	// reconciles their owner when they or their allocations change.
	OwnsJobs bool

	// Sources are additional change sources, for example an EventSource.
	Sources []Source

	// Predicate filters object updates; SpecChanged if nil.
	Predicate Predicate

	// LeaderElection, if set, runs the reconciles and the sources only on
	// the elected leader. The informer runs on every candidate, so a new
	// leader starts with a warm cache.
	LeaderElection *LeaderElectionConfig

	Metrics Metrics
	Logger  *slog.Logger
}

// Controller runs a Reconciler for the objects of a kind.
type Controller struct {
	opts       Options
	backend    Backend
	reconciler Reconciler
	logger     *slog.Logger

	informer *Informer
	objects  *Objects
	elector  *LeaderElector

	mu    sync.RWMutex
	queue *Queue[ObjectKey] // nil when not reconciling (not the leader)
}

// New returns a controller. Run starts it.
func New(backend Backend, reconciler Reconciler, opts Options) (*Controller, error) {
	if err := ValidateName(opts.Name); err != nil {
		return nil, fmt.Errorf("invalid controller name: %w", err)
	}
	if err := opts.Kind.Validate(); err != nil {
		return nil, err
	}
	if reconciler == nil {
		return nil, errors.New("a reconciler is required")
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.RateLimiter == nil {
		opts.RateLimiter = DefaultRateLimiter[ObjectKey]()
	}
	if opts.ResyncPeriod == 0 {
		opts.ResyncPeriod = 10 * time.Minute
	}
	if opts.Predicate == nil {
		opts.Predicate = SpecChanged
	}
	if opts.Metrics == nil {
		opts.Metrics = NopMetrics{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	logger := opts.Logger.With("controller", opts.Name)

	c := &Controller{
		opts:       opts,
		backend:    backend,
		reconciler: reconciler,
		logger:     logger,
	}

	var err error
	c.objects, err = NewObjects(opts.Kind, backend)
	if err != nil {
		return nil, err
	}
	c.informer, err = NewInformer(opts.Kind, backend, backend, InformerOptions{
		Namespace:    opts.Namespace,
		ResyncPeriod: max(opts.ResyncPeriod, 0),
		WatchMode:    opts.WatchMode,
		Name:         opts.Name,
		Logger:       logger,
		Metrics:      opts.Metrics,
	})
	if err != nil {
		return nil, err
	}
	c.informer.AddHandler(EventHandler{
		OnAdd: func(obj *Object) { c.Enqueue(obj.Key) },
		OnUpdate: func(oldObj, newObj *Object) {
			if c.opts.Predicate(oldObj, newObj) {
				c.Enqueue(newObj.Key)
			}
		},
		OnDelete: func(obj *Object) { c.Enqueue(obj.Key) },
	})

	if opts.LeaderElection != nil {
		le := *opts.LeaderElection
		if le.Path == "" {
			le.Path = LeaseRoot + "/" + opts.Name
		}
		if le.Namespace == "" && opts.Namespace != "*" {
			le.Namespace = opts.Namespace
		}
		c.elector, err = NewLeaderElector(backend, le, logger)
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Name returns the name of the controller.
func (c *Controller) Name() string { return c.opts.Name }

// Kind returns the kind of the objects of the controller.
func (c *Controller) Kind() Kind { return c.opts.Kind }

// Backend returns the backend of the controller.
func (c *Controller) Backend() Backend { return c.backend }

// Objects returns the object client of the controller's kind.
func (c *Controller) Objects() *Objects { return c.objects }

// Informer returns the informer of the controller's objects.
func (c *Controller) Informer() *Informer { return c.informer }

// Get returns the cached object, or false if it does not exist.
func (c *Controller) Get(key ObjectKey) (*Object, bool) { return c.informer.Get(key) }

// IsLeader reports whether the controller reconciles: it is the leader, or
// runs without leader election.
func (c *Controller) IsLeader() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.queue != nil
}

// Enqueue requests reconciling the object. It is a no-op while the
// controller is not the leader.
func (c *Controller) Enqueue(key ObjectKey) {
	c.mu.RLock()
	q := c.queue
	c.mu.RUnlock()
	if q != nil {
		q.Add(key)
		c.opts.Metrics.QueueDepth(c.opts.Name, q.Len())
	}
}

// EnqueueAll requests reconciling every cached object.
func (c *Controller) EnqueueAll() {
	for _, key := range c.informer.Keys() {
		c.Enqueue(key)
	}
}

// Run runs the controller until ctx is canceled.
func (c *Controller) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.informer.Run(ctx)
	}()

	if c.opts.ResyncPeriod > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(c.opts.ResyncPeriod)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c.EnqueueAll()
				}
			}
		}()
	}

	if !c.informer.WaitForSync(ctx) {
		wg.Wait()
		return ctx.Err()
	}
	c.logger.Info("cache synced", "objects", len(c.informer.Keys()))

	if c.elector != nil {
		c.elector.Run(ctx, c.reconcileLoop, nil)
	} else {
		c.reconcileLoop(ctx)
	}
	cancel()
	wg.Wait()
	return nil
}

// reconcileLoop runs the workers until ctx is canceled.
func (c *Controller) reconcileLoop(ctx context.Context) {
	q := NewQueue(c.opts.RateLimiter)
	c.mu.Lock()
	c.queue = q
	c.mu.Unlock()
	c.opts.Metrics.LeaderChanged(c.opts.Name, true)
	c.logger.Info("reconciling")

	// Everything may have changed while not reconciling.
	c.EnqueueAll()

	// The sources run only while reconciling: the keys they produce that
	// are not objects (yet) would be lost otherwise. They list on start.
	var wg sync.WaitGroup
	sc := &SourceContext{
		Enqueue:    c.Enqueue,
		EnqueueAll: c.EnqueueAll,
		Objects:    c.informer.List,
		watcher: &watcher{
			events:     c.backend,
			mode:       c.opts.WatchMode,
			logger:     c.logger,
			metrics:    c.opts.Metrics,
			controller: c.opts.Name,
		},
		backend: c.backend,
		kind:    c.opts.Kind,
		ns:      c.opts.Namespace,
	}
	sources := c.opts.Sources
	if c.opts.OwnsJobs {
		sources = append([]Source{&OwnedJobsSource{}}, sources...)
	}
	for _, src := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src.Run(ctx, sc)
		}()
	}

	go func() {
		<-ctx.Done()
		q.ShutDown()
	}()

	for range c.opts.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				key, shutdown := q.Get()
				if shutdown {
					return
				}
				c.process(ctx, q, key)
			}
		}()
	}
	wg.Wait()

	c.mu.Lock()
	c.queue = nil
	c.mu.Unlock()
	c.opts.Metrics.LeaderChanged(c.opts.Name, false)
	c.logger.Info("stopped reconciling")
}

func (c *Controller) process(ctx context.Context, q *Queue[ObjectKey], key ObjectKey) {
	defer q.Done(key)
	start := time.Now()
	requeueAfter, err := c.reconcile(ctx, key)
	d := time.Since(start)

	result := ResultSuccess
	switch {
	case err != nil:
		result = ResultError
		if ctx.Err() == nil {
			c.logger.Error("reconcile failed", "object", key.String(), "retries", q.NumRequeues(key), "error", err)
			q.AddRateLimited(key)
		}
	case requeueAfter > 0:
		result = ResultRequeue
		q.Forget(key)
		q.AddAfter(key, requeueAfter)
	default:
		q.Forget(key)
	}
	c.logger.Debug("reconciled", "object", key.String(), "duration", d, "result", result)
	c.opts.Metrics.ReconcileDone(c.opts.Name, d, result)
	c.opts.Metrics.QueueDepth(c.opts.Name, q.Len())
}

func (c *Controller) reconcile(ctx context.Context, key ObjectKey) (requeueAfter time.Duration, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("reconcile panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return c.reconciler.Reconcile(ctx, key)
}
