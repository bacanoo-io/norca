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
//     is kept for a retention period, and until its dependents are gone or
//     marked for deletion, so dependents created late (by a controller
//     racing the deletion) are collected too. It does not wait for a
//     dependent that a finalizer holds: that one is marked, and goes as any
//     object that is being deleted. The event log of the deleted object is
//     kept with it.
//
// An object depends on an owner when its Owners reference the owner (kind,
// namespace and name); it is deleted only when all its custom object owners
// are gone. A job depends on an owner when its meta references the owner
// under the reserved key structs.JobMetaOwner. Jobs are stopped, not purged;
// the job garbage collector purges them later. The tombstone of an object
// deleted in the orphan mode only keeps its event log for the retention: no
// dependent is collected for it, so what the object owned stays.
//
// Dependents of a deleted owner are never collected while an object with the
// owner's kind, namespace and name exists again (a re-created or restored
// owner adopts them).
//
// The collector acts with the rights of the servers and has no caller. A
// dependent whose deletion does more than remove the object (Deps.Guarded: it
// destroys data in the storage) is deleted only if the deletion of its owner
// names it (CustomObject.DeletionApproved, ObjectTombstone.ApprovedDependents):
// whoever deleted the owner was checked for the right to delete that
// dependent then. The list goes on to the deletions the collector makes
// itself, so it holds through objects in between. A guarded dependent that is
// not named (it was written after the check, or the owner was deleted by the
// servers) is let go of instead: its reference to the owner is removed, and
// it stays.
//
// The same holds for every job: stopping a job needs a right, so a job is
// stopped only if the list names it (structs.ObjectApprovedJob). A job that
// is not named keeps running. Nothing of it is changed: its owner is in its
// meta, and a job whose meta changes is restarted. Deps.KeepJob is told once.
package objectgc

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	hclog "github.com/hashicorp/go-hclog"
	memdb "github.com/hashicorp/go-memdb"
)

// Config configures the garbage collector.
type Config struct {
	// Interval is how often pending work (what a pass deleted or stopped,
	// jobs stopping, a failed step) is checked again. Changes of objects and
	// tombstones wake the collector up at once, so an object that only waits
	// for a finalizer is not pending work.
	Interval time.Duration

	// IdleInterval is how often the collector checks without changes.
	IdleInterval time.Duration

	// TombstoneRetention is how long a tombstone, and the event log of its
	// object, is kept after the deletion of the object, at least: it is
	// removed by the first pass after that, which comes within IdleInterval.
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
	// index of obj. approved is recorded with the deletion: the guarded
	// dependents of obj the collector may delete in turn.
	DeleteObject func(obj *structs.CustomObject, approved []string) error

	// Guarded reports whether the deletion of the object does more than
	// remove it, so that it needs rights of its own. Optional: without it
	// no object is guarded.
	Guarded func(obj *structs.CustomObject) bool

	// ReleaseObject removes the references of a guarded dependent to an
	// owner that was deleted without the dependent being approved, and
	// records why it stays. It checks the modify index of obj. It is
	// needed with Guarded.
	ReleaseObject func(obj *structs.CustomObject, ownerKindRef, ownerNamespace, ownerName string) error

	// RemoveObject removes an object marked for deletion, with check-and-set
	// on its modify index, leaving a tombstone for the propagation mode.
	RemoveObject func(obj *structs.CustomObject, propagation string) error

	// StopJob stops (deregisters without purging) a job.
	StopJob func(namespace, id string) error

	// KeepJob is told of a dependent job that is not stopped with its
	// owner, because the deletion of the owner does not name it: once for a
	// job and an owner while the collector runs. Optional.
	KeepJob func(job *structs.Job, ownerKindRef, ownerNamespace, ownerName string)

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

	// keptJobs are the jobs the last pass did not stop with their owner
	// (by owner and job): KeepJob was told of them.
	keptJobs map[string]struct{}
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

	// tombstone is set for the tombstone of a deleted object, which waits
	// for nothing that is under way already.
	tombstone bool

	// approved are the IDs of the guarded dependents that the deletion of
	// the owner may take with it.
	approved []string
}

func (o owner) ref() string {
	return structs.ObjectKindRef(o.group, o.kind) + "\x00" + o.namespace + "\x00" + o.name
}

func objectRef(obj *structs.CustomObject) string {
	return obj.KindRef() + "\x00" + obj.Namespace + "\x00" + obj.Name
}

// world is what one pass sees of the state: a snapshot, read through the
// owner indexes, so a pass reads the objects being deleted, the tombstones
// and their dependents, not every object and job (review finding MUK-13).
type world struct {
	snap *state.StateSnapshot

	// keptJobs are the jobs this pass does not stop with their owner.
	keptJobs map[string]struct{}
}

// object returns the object of a "group/Kind" reference, namespace and name,
// or nil.
func (w *world) object(kindRef, namespace, name string) (*structs.CustomObject, error) {
	group, kind, err := structs.ParseObjectKindRef(kindRef)
	if err != nil {
		return nil, nil
	}
	return w.snap.CustomObjectByName(nil, group, kind, namespace, name)
}

// Collect runs one pass of the collector. It returns whether work is still
// pending. The watch set is notified when objects or tombstones change.
func (c *Collector) Collect(ws memdb.WatchSet, store *state.StateStore) (bool, error) {
	snap, err := store.Snapshot()
	if err != nil {
		return true, err
	}

	w := &world{snap: snap, keptJobs: map[string]struct{}{}}
	defer func() {
		c.l.Lock()
		c.keptJobs = w.keptJobs
		c.l.Unlock()
	}()
	// Any change of the objects wakes the collector up: a dependent
	// written late, a finalizer removed.
	if err := snap.WatchCustomObjects(ws); err != nil {
		return true, err
	}
	var deleting []*structs.CustomObject
	iter, err := snap.CustomObjectsDeleting(ws)
	if err != nil {
		return true, err
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		deleting = append(deleting, raw.(*structs.CustomObject))
	}

	var tombstones []*structs.ObjectTombstone
	iter, err = snap.ObjectTombstones(ws)
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
	pending := false
	now := c.now()

	for _, obj := range deleting {
		o := owner{group: obj.Group, kind: obj.Kind, namespace: obj.Namespace, name: obj.Name,
			foreground: obj.DeletionPropagation == structs.ObjectPropagationForeground,
			approved:   obj.DeletionApproved}
		remaining, poll := 0, false
		if o.foreground {
			if remaining, poll, err = c.collectDependents(w, o); err != nil {
				return true, err
			}
		}
		if poll {
			pending = true
		}
		// An object that waits for its finalizers, or for a dependent that
		// waits for its own, is not pending work: the removal of a
		// finalizer and of an object are changes of the objects, which wake
		// the collector (review finding MUK-29).
		if remaining > 0 || len(obj.Finalizers) > 0 {
			continue
		}
		if err := c.deps.RemoveObject(obj, obj.DeletionPropagation); err != nil {
			// A conflict: the object changed or was removed by its
			// operator meanwhile; the next pass sees which.
			level := c.logger.Warn
			if structs.IsObjectConflictError(err) {
				level = c.logger.Debug
			}
			level("failed to remove deleted object", "object_kind", obj.KindRef(), "object_name", obj.Name,
				"namespace", obj.Namespace, "error", err)
			pending = true
		}
	}

	var done []string
	for _, t := range tombstones {
		o := owner{group: t.Group, kind: t.Kind, namespace: t.Namespace, name: t.Name,
			foreground: t.Propagation == structs.ObjectPropagationForeground,
			tombstone:  true,
			approved:   t.ApprovedDependents}
		remaining, poll := 0, false
		// A re-created owner adopts the dependents. The tombstone of an
		// orphan deletion takes none: it only keeps the event log of the
		// object for the retention.
		again, err := w.object(t.KindRef(), t.Namespace, t.Name)
		if err != nil {
			return true, err
		}
		if again == nil && t.Propagation != structs.ObjectPropagationOrphan {
			if remaining, poll, err = c.collectDependents(w, o); err != nil {
				return true, err
			}
		}
		if poll {
			pending = true
		}
		if remaining > 0 {
			continue
		}
		// A tombstone that only waits for its retention is not pending
		// work: a pass after the idle interval removes it, and a change
		// of the objects wakes the collector for late dependents.
		if now.Sub(time.Unix(0, t.DeleteTime)) < c.config().TombstoneRetention {
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
// returns how many of them o still waits for. In the foreground mode a job
// counts until it is stopped and dead, otherwise until it is stopped.
//
// A dependent object that is marked for deletion already (a finalizer holds
// it) counts for an object that is being deleted in the foreground mode,
// which is removed after its dependents. It does not count for a tombstone
// (review finding MUK-29): the mark is all the deletion of the owner asks for
// that dependent. The collector removes it when its finalizers are gone, as
// any object that is being deleted, and its own tombstone takes its
// dependents, with the approved list of its mark. A tombstone that waited for
// it would stay for as long as the finalizer does.
//
// poll reports that the next pass should come on the short interval: the
// pass did something (the next one sees what came of it), something failed,
// or o waits for a job, and jobs are not watched. Waiting for a dependent
// that a finalizer holds is not such a reason: the removal of the finalizer
// and of the dependent are changes of the objects, which wake the collector.
func (c *Collector) collectDependents(w *world, o owner) (remaining int, poll bool, err error) {
	deps, err := w.snap.CustomObjectsByOwner(nil, structs.ObjectKindRef(o.group, o.kind), o.namespace, o.name)
	if err != nil {
		return 0, false, err
	}
	for raw := deps.Next(); raw != nil; raw = deps.Next() {
		dep := raw.(*structs.CustomObject)
		if !structs.ObjectOwnedBy(dep, o.group, o.kind, o.namespace, o.name) {
			continue
		}
		if gone, err := c.ownersGone(w, dep, o); err != nil {
			return 0, false, err
		} else if !gone {
			continue
		}
		// An object is not its own dependent, and neither is one of its
		// owners that waits for it in a foreground deletion of its own:
		// they would wait for each other forever.
		if objectRef(dep) == o.ref() {
			continue
		}
		if o.foreground {
			if waits, err := waitsFor(w, dep, o); err != nil {
				return 0, false, err
			} else if waits {
				continue
			}
		}
		if dep.Deleting() {
			if !o.tombstone {
				remaining++
			}
			continue
		}
		// Nobody was checked for the right to delete this dependent with
		// its owner: it is not deleted, and no longer waited for.
		if c.deps.Guarded != nil && c.deps.Guarded(dep) && !slices.Contains(o.approved, dep.ID) {
			c.logger.Info("dependent object is not deleted with its owner: its deletion was not approved",
				"owner", o.name, "object_kind", dep.KindRef(), "object_name", dep.Name, "namespace", dep.Namespace)
			if err := c.deps.ReleaseObject(dep, structs.ObjectKindRef(o.group, o.kind), o.namespace, o.name); err != nil {
				level := c.logger.Warn
				if structs.IsObjectConflictError(err) {
					level = c.logger.Debug
				}
				level("failed to release dependent object", "object_kind", dep.KindRef(), "object_name", dep.Name,
					"namespace", dep.Namespace, "error", err)
				remaining++
			}
			poll = true
			continue
		}
		remaining++
		poll = true
		c.logger.Debug("deleting dependent object", "owner", o.name, "object_kind", dep.KindRef(), "object_name", dep.Name,
			"namespace", dep.Namespace)
		if err := c.deps.DeleteObject(dep, o.approved); err != nil {
			c.logger.Warn("failed to delete dependent object", "object_kind", dep.KindRef(), "object_name", dep.Name,
				"namespace", dep.Namespace, "error", err)
		}
	}

	jobs, err := w.snap.JobsByOwner(nil, structs.ObjectOwnerMetaValue(o.group, o.kind, o.namespace, o.name))
	if err != nil {
		return 0, false, err
	}
	for raw := jobs.Next(); raw != nil; raw = jobs.Next() {
		job := raw.(*structs.Job)
		if !structs.JobOwnedBy(job.Meta, o.group, o.kind, o.namespace, o.name) {
			continue
		}
		if job.Stop {
			if o.foreground && job.Status != structs.JobStatusDead {
				remaining++
				poll = true
			}
			continue
		}
		// Nobody was checked for the right to stop this job with its
		// owner: it keeps running, and is not waited for.
		if !slices.Contains(o.approved, structs.ObjectApprovedJob(job.Namespace, job.ID, job.CreateIndex)) {
			c.keepJob(w, o, job)
			continue
		}
		remaining++
		poll = true
		c.logger.Debug("stopping dependent job", "owner", o.name, "job_id", job.ID, "namespace", job.Namespace)
		if err := c.deps.StopJob(job.Namespace, job.ID); err != nil {
			c.logger.Warn("failed to stop dependent job", "job_id", job.ID, "namespace", job.Namespace, "error", err)
		}
	}
	return remaining, poll, nil
}

// keepJob notes a dependent job that is not stopped with its owner, and
// tells of it the first time.
func (c *Collector) keepJob(w *world, o owner, job *structs.Job) {
	key := o.ref() + "\x00" + job.Namespace + "\x00" + job.ID
	w.keptJobs[key] = struct{}{}
	c.l.Lock()
	_, told := c.keptJobs[key]
	c.l.Unlock()
	if told {
		return
	}
	c.logger.Warn("dependent job is not stopped with its owner: the deletion of the owner was not approved for it",
		"owner", structs.ObjectKindRef(o.group, o.kind)+"/"+o.name, "owner_namespace", o.namespace,
		"job_id", job.ID, "namespace", job.Namespace)
	if c.deps.KeepJob != nil {
		c.deps.KeepJob(job, structs.ObjectKindRef(o.group, o.kind), o.namespace, o.name)
	}
}

// waitsFor reports whether dep is being deleted in the foreground mode and
// o is one of its dependents, directly or through other objects: dep is then
// not removed before o is gone.
func waitsFor(w *world, dep *structs.CustomObject, o owner) (bool, error) {
	if !dep.Deleting() || dep.DeletionPropagation != structs.ObjectPropagationForeground {
		return false, nil
	}
	type ref struct{ kind, namespace, name string }
	target := objectRef(dep)
	seen := map[string]struct{}{}
	queue := []ref{{structs.ObjectKindRef(o.group, o.kind), o.namespace, o.name}}
	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]
		key := r.kind + "\x00" + r.namespace + "\x00" + r.name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		obj, err := w.object(r.kind, r.namespace, r.name)
		if err != nil {
			return false, err
		}
		if obj == nil {
			continue
		}
		for _, up := range obj.Owners {
			if !up.IsObject() {
				continue
			}
			if up.Kind+"\x00"+up.Namespace+"\x00"+up.Name == target {
				return true, nil
			}
			queue = append(queue, ref{up.Kind, up.Namespace, up.Name})
		}
	}
	return false, nil
}

// ownersGone reports whether every custom object owner of dep other than o
// is gone, so dep can be collected for o.
func (c *Collector) ownersGone(w *world, dep *structs.CustomObject, o owner) (bool, error) {
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
		other, err := w.snap.CustomObjectByName(nil, group, kind, ref.Namespace, ref.Name)
		if err != nil {
			return false, err
		}
		if other != nil && !other.Deleting() {
			return false, nil
		}
	}
	return true, nil
}
