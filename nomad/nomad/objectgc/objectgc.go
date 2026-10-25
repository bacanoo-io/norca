// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package objectgc is the garbage collector of custom objects (operator
// framework). It runs on the leader and completes the deletion of objects:
//
//   - Objects marked for deletion in the foreground mode: the objects and
//     jobs they own are deleted first, then the object is removed once its
//     finalizers are gone.
//   - Objects marked for deletion whose finalizers are gone are removed (the
//     finalizers endpoint usually does this itself).
//   - Tombstones of objects deleted in the background or foreground mode:
//     the objects and jobs the deleted object owned are deleted. A tombstone
//     is kept for a retention period after its dependents are gone, so
//     dependents created late (by a controller racing the deletion) are
//     collected too.
//
// An object depends on an owner when its Owners reference the owner (kind,
// namespace and name); it is deleted only when all its custom object owners
// are gone. A job depends on an owner when its meta references the owner
// under the reserved key structs.JobMetaOwner. Jobs are stopped, not purged;
// the job garbage collector purges them later. Objects deleted in the orphan
// mode leave no tombstone, so what they owned stays.
//
// Dependents of a deleted owner are never collected while an object with the
// owner's kind, namespace and name exists again (a re-created or restored
// owner adopts them).
package objectgc

import (
	"context"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Config configures the garbage collector.
type Config struct {
	// Interval is how often pending work (dependents being deleted, jobs
	// stopping) is checked again. Changes of objects and tombstones wake the
	// collector up at once.
	Interval time.Duration

	// IdleInterval is how often the collector checks without changes.
	IdleInterval time.Duration

	// TombstoneRetention is how long a tombstone is kept after the deletion
	// of its object, at least.
	TombstoneRetention time.Duration
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		Interval:           5 * time.Second,
		IdleInterval:       time.Minute,
		TombstoneRetention: 5 * time.Minute,
	}
}

// Deps are the server facilities the collector uses.
type Deps struct {
	// State returns the current state store.
	State func() *state.StateStore

	// DeleteObject deletes a dependent object like a deletion in the
	// background mode: an object with finalizers is marked for deletion,
	// one without is removed (leaving a tombstone). It checks the modify
	// index of obj.
	DeleteObject func(obj *structs.CustomObject) error

	// RemoveObject removes an object marked for deletion, with check-and-set
	// on its modify index, leaving a tombstone for the propagation mode.
	RemoveObject func(obj *structs.CustomObject, propagation string) error

	// StopJob stops (deregisters without purging) a job.
	StopJob func(namespace, id string) error

	// DeleteTombstones removes processed tombstones.
	DeleteTombstones func(ids []string) error

	// ServersReady reports whether all servers support the object
	// lifecycle. Optional.
	ServersReady func() bool

	// Now returns the current time. Optional.
	Now func() time.Time
}

// Collector is the object garbage collector.
type Collector struct {
	logger hclog.Logger
	cfg    *Config
	deps   *Deps
	now    func() time.Time

	l      sync.Mutex
	cancel context.CancelFunc
	doneCh chan struct{}

	// kick wakes up the run loop.
	kick chan struct{}
}

// New returns a stopped collector.
func New(logger hclog.Logger, cfg *Config, deps *Deps) *Collector {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Collector{
		logger: logger.Named("object_gc"),
		cfg:    cfg,
		deps:   deps,
		now:    now,
		kick:   make(chan struct{}, 1),
	}
}

// SetEnabled starts the collector on the leader and stops it otherwise.
func (c *Collector) SetEnabled(enabled bool) {
	c.l.Lock()
	defer c.l.Unlock()

	switch {
	case enabled && c.cancel == nil:
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		c.doneCh = make(chan struct{})
		go c.run(ctx, c.doneCh)
	case !enabled && c.cancel != nil:
		c.cancel()
		c.cancel = nil
	}
}

// SetConfig replaces the configuration. It takes effect with the next pass.
func (c *Collector) SetConfig(cfg *Config) {
	c.l.Lock()
	c.cfg = cfg
	c.l.Unlock()
	c.Kick()
}

func (c *Collector) config() *Config {
	c.l.Lock()
	defer c.l.Unlock()
	return c.cfg
}

// Running reports whether the collector runs.
func (c *Collector) Running() bool {
	c.l.Lock()
	defer c.l.Unlock()
	return c.cancel != nil
}

// Kick runs a collection pass soon.
func (c *Collector) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Collector) run(ctx context.Context, doneCh chan struct{}) {
	defer close(doneCh)

	for c.deps.ServersReady != nil && !c.deps.ServersReady() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.config().Interval):
		}
	}

	for {
		store := c.deps.State()
		ws := memdb.NewWatchSet()
		ws.Add(store.AbandonCh())

		pending, err := c.Collect(ws, store)
		if err != nil {
			c.logger.Error("object garbage collection failed", "error", err)
			pending = true
		}

		cfg := c.config()
		wait := cfg.IdleInterval
		if pending {
			wait = cfg.Interval
		}
		waitCtx, cancel := context.WithTimeout(ctx, wait)
		go func() {
			select {
			case <-c.kick:
				cancel()
			case <-waitCtx.Done():
			}
		}()
		_ = ws.WatchCtx(waitCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
}

// owner is an object whose dependents are collected: an object being
// deleted in the foreground mode or the tombstone of a deleted object.
type owner struct {
	group, kind, namespace, name string
	foreground                   bool
}

func (o owner) ref() string {
	return structs.ObjectKindRef(o.group, o.kind) + "\x00" + o.namespace + "\x00" + o.name
}

func objectRef(obj *structs.CustomObject) string {
	return obj.KindRef() + "\x00" + obj.Namespace + "\x00" + obj.Name
}

// world is what one pass sees of the state.
type world struct {
	objects map[string]*structs.CustomObject
	jobs    []*structs.Job
}

// Collect runs one pass of the collector. It returns whether work is still
// pending. The watch set is notified when objects or tombstones change.
func (c *Collector) Collect(ws memdb.WatchSet, store *state.StateStore) (bool, error) {
	snap, err := store.Snapshot()
	if err != nil {
		return true, err
	}

	w := &world{objects: map[string]*structs.CustomObject{}}
	var deleting []*structs.CustomObject
	kinds, err := snap.ObjectKinds(ws, "", state.SortDefault)
	if err != nil {
		return true, err
	}
	for raw := kinds.Next(); raw != nil; raw = kinds.Next() {
		kind := raw.(*structs.ObjectKind)
		iter, err := snap.CustomObjects(ws, kind.Group, kind.Kind, structs.AllNamespacesSentinel, state.SortDefault)
		if err != nil {
			return true, err
		}
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			obj := raw.(*structs.CustomObject)
			w.objects[objectRef(obj)] = obj
			if obj.Deleting() {
				deleting = append(deleting, obj)
			}
		}
	}

	var tombstones []*structs.ObjectTombstone
	iter, err := snap.ObjectTombstones(ws)
	if err != nil {
		return true, err
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		tombstones = append(tombstones, raw.(*structs.ObjectTombstone))
	}

	if len(deleting) == 0 && len(tombstones) == 0 {
		return false, nil
	}

	// Jobs are read without watching them: pending work is checked again
	// on the interval.
	jobs, err := snap.Jobs(nil)
	if err != nil {
		return true, err
	}
	for raw := jobs.Next(); raw != nil; raw = jobs.Next() {
		job := raw.(*structs.Job)
		if _, ok := job.Meta[structs.JobMetaOwner]; ok {
			w.jobs = append(w.jobs, job)
		}
	}

	pending := false
	now := c.now()

	for _, obj := range deleting {
		o := owner{group: obj.Group, kind: obj.Kind, namespace: obj.Namespace, name: obj.Name,
			foreground: obj.DeletionPropagation == structs.ObjectPropagationForeground}
		remaining := 0
		if o.foreground {
			remaining = c.collectDependents(w, o)
		}
		if remaining > 0 || len(obj.Finalizers) > 0 {
			pending = true
			continue
		}
		if err := c.deps.RemoveObject(obj, obj.DeletionPropagation); err != nil {
			c.logger.Warn("failed to remove deleted object", "object", obj.KindRef()+"/"+obj.Name,
				"namespace", obj.Namespace, "error", err)
			pending = true
		}
	}

	var done []string
	for _, t := range tombstones {
		o := owner{group: t.Group, kind: t.Kind, namespace: t.Namespace, name: t.Name,
			foreground: t.Propagation == structs.ObjectPropagationForeground}
		remaining := 0
		// A re-created owner adopts the dependents.
		if _, exists := w.objects[o.ref()]; !exists {
			remaining = c.collectDependents(w, o)
		}
		if remaining > 0 || now.Sub(time.Unix(0, t.DeleteTime)) < c.config().TombstoneRetention {
			pending = true
			continue
		}
		done = append(done, t.ID)
	}
	if len(done) > 0 {
		if err := c.deps.DeleteTombstones(done); err != nil {
			return true, err
		}
	}
	return pending, nil
}

// collectDependents deletes the objects and stops the jobs owned by o, and
// returns how many of them are not gone yet. In the foreground mode a job
// counts until it is stopped and dead, otherwise until it is stopped.
func (c *Collector) collectDependents(w *world, o owner) int {
	remaining := 0
	for _, dep := range w.objects {
		if !structs.ObjectOwnedBy(dep, o.group, o.kind, o.namespace, o.name) || !c.ownersGone(w, dep, o) {
			continue
		}
		remaining++
		if dep.Deleting() {
			continue
		}
		c.logger.Debug("deleting dependent object", "owner", o.name, "object", dep.KindRef()+"/"+dep.Name,
			"namespace", dep.Namespace)
		if err := c.deps.DeleteObject(dep); err != nil {
			c.logger.Warn("failed to delete dependent object", "object", dep.KindRef()+"/"+dep.Name,
				"namespace", dep.Namespace, "error", err)
		}
	}

	for _, job := range w.jobs {
		if !structs.JobOwnedBy(job.Meta, o.group, o.kind, o.namespace, o.name) {
			continue
		}
		if job.Stop {
			if o.foreground && job.Status != structs.JobStatusDead {
				remaining++
			}
			continue
		}
		remaining++
		c.logger.Debug("stopping dependent job", "owner", o.name, "job", job.ID, "namespace", job.Namespace)
		if err := c.deps.StopJob(job.Namespace, job.ID); err != nil {
			c.logger.Warn("failed to stop dependent job", "job", job.ID, "namespace", job.Namespace, "error", err)
		}
	}
	return remaining
}

// ownersGone reports whether every custom object owner of dep other than o
// is gone, so dep can be collected for o.
func (c *Collector) ownersGone(w *world, dep *structs.CustomObject, o owner) bool {
	for _, ref := range dep.Owners {
		if !ref.IsObject() {
			continue
		}
		group, kind, err := structs.ParseObjectKindRef(ref.Kind)
		if err != nil {
			continue
		}
		if group == o.group && kind == o.kind && ref.Namespace == o.namespace && ref.Name == o.name {
			continue
		}
		if other, ok := w.objects[structs.ObjectKindRef(group, kind)+"\x00"+ref.Namespace+"\x00"+ref.Name]; ok && !other.Deleting() {
			return false
		}
	}
	return true
}
