// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"strings"
	"sync"

	"github.com/hashicorp/nomad/api"
)

// OwnerMetaKey is the reserved job meta key holding the owner reference of
// jobs created by a controller. Its value is "<namespace>/<object path>",
// for example "default/crd/norca-io/LoadBalancer/web". OwnerKindMetaKey
// holds the "group/Kind" of the owner. The server's object garbage
// collector stops the jobs of deleted native objects (except with the
// orphan deletion mode).
const (
	OwnerMetaKey     = api.JobMetaOwner
	OwnerKindMetaKey = api.JobMetaOwnerKind
)

// OwnerRef returns the owner reference of the object, the value of
// OwnerMetaKey in the meta of the jobs it owns.
func OwnerRef(obj *Object) string {
	return obj.Key.Namespace + "/" + obj.Path()
}

// SetOwner marks the job as owned by the object.
func SetOwner(job *api.Job, obj *Object) {
	if job.Meta == nil {
		job.Meta = map[string]string{}
	}
	job.Meta[OwnerMetaKey] = OwnerRef(obj)
	job.Meta[OwnerKindMetaKey] = obj.Kind.String()
}

// OwnerFromMeta returns the key of the owner object of the kind recorded in
// the meta of a job, if any.
func OwnerFromMeta(kind Kind, meta map[string]string) (ObjectKey, bool) {
	ref, ok := meta[OwnerMetaKey]
	if !ok {
		return ObjectKey{}, false
	}
	if k, ok := meta[OwnerKindMetaKey]; ok && k != kind.String() {
		return ObjectKey{}, false
	}
	ns, path, ok := strings.Cut(ref, "/")
	if !ok || ns == "" {
		return ObjectKey{}, false
	}
	name, ok := kind.NameFromPath(path)
	if !ok {
		return ObjectKey{}, false
	}
	return ObjectKey{Namespace: ns, Name: name}, true
}

// IsOwnedBy reports whether the job meta names obj as its owner.
func IsOwnedBy(meta map[string]string, obj *Object) bool {
	if k, ok := meta[OwnerKindMetaKey]; ok && k != obj.Kind.String() {
		return false
	}
	return meta[OwnerMetaKey] == OwnerRef(obj)
}

// ListOwnedJobs lists the jobs of namespace owned by the object. Stopped jobs
// are included.
func ListOwnedJobs(ctx context.Context, jobs JobsAPI, namespace string, obj *Object) ([]*api.JobListStub, error) {
	stubs, _, err := jobs.ListJobs(ctx, namespace, 0)
	if err != nil {
		return nil, err
	}
	var out []*api.JobListStub
	for _, stub := range stubs {
		if IsOwnedBy(stub.Meta, obj) {
			out = append(out, stub)
		}
	}
	return out, nil
}

// Source produces keys to reconcile from changes outside the controller's
// own objects. Run must block until ctx is canceled. It calls enqueue for
// single keys, and enqueueAll when it may have missed changes (on start and
// after resynchronizations), which reconciles every object.
type Source interface {
	Name() string
	Run(ctx context.Context, w *SourceContext)
}

// SourceContext is passed to Sources by the controller.
type SourceContext struct {
	// Enqueue requests reconciling one object.
	Enqueue func(ObjectKey)
	// EnqueueAll requests reconciling every object of the controller.
	EnqueueAll func()
	// Objects returns the cached objects of the controller (for mapping
	// changes to objects, for example by a field of their spec).
	Objects func() []*Object

	watcher *watcher
	backend Backend
	kind    Kind
	ns      string
}

// Watch runs a list + watch loop for a source: list is called on start and
// after resynchronizations (EnqueueAll is called after it) and returns the
// index to resume from (0: the newest events); apply is called for every
// event.
func (s *SourceContext) Watch(ctx context.Context, name string, topics map[api.Topic][]string,
	list func(ctx context.Context) (uint64, error), apply func(ctx context.Context, ev *api.Event) error) {
	s.watcher.run(ctx, watchConfig{
		name:      name,
		namespace: s.ns,
		topics:    topics,
		list: func(ctx context.Context) (uint64, error) {
			index, err := list(ctx)
			if err != nil {
				return 0, err
			}
			s.EnqueueAll()
			return index, nil
		},
		apply:            apply,
		fromNowAfterGone: true,
	})
}

// EventSource maps events of arbitrary topics to objects. It has no listing:
// it starts with the newest events and requests reconciling every object
// when it may have missed changes.
type EventSource struct {
	// SourceName identifies the source in logs and metrics.
	SourceName string

	// Topics to subscribe to, in the controller's namespace.
	Topics map[api.Topic][]string

	// Map returns the keys of the objects affected by an event. It may use
	// SourceContext.Objects to search the cached objects.
	Map func(ev *api.Event, sc *SourceContext) []ObjectKey
}

func (s *EventSource) Name() string { return s.SourceName }

func (s *EventSource) Run(ctx context.Context, sc *SourceContext) {
	if sc.watcher.mode == WatchPoll {
		// Without the event stream only the periodic resynchronization of
		// the controller covers the source.
		<-ctx.Done()
		return
	}
	sc.Watch(ctx, s.SourceName, s.Topics,
		func(context.Context) (uint64, error) { return 0, nil },
		func(_ context.Context, ev *api.Event) error {
			for _, key := range s.Map(ev, sc) {
				sc.Enqueue(key)
			}
			return nil
		})
}

// OwnedJobsSource maps the changes of jobs owned by objects of the
// controller's kind (by OwnerMetaKey) to their owners. It watches the Job
// topic, and the Allocation, Evaluation and Deployment topics through an
// index of owned job IDs, so allocation state changes also reach the owner,
// and so do purged jobs on older servers (Nomad 1.6.5), where a purge
// produces no Job event.
//
// In WatchPoll mode it polls the job list with blocking queries instead and
// maps jobs whose ModifyIndex changed or that disappeared.
type OwnedJobsSource struct {
	mu    sync.Mutex
	owned map[string]ownedJob // namespace/jobID -> owner
}

type ownedJob struct {
	owner       ObjectKey
	modifyIndex uint64
}

func (s *OwnedJobsSource) Name() string { return "owned-jobs" }

func jobKey(namespace, id string) string { return namespace + "/" + id }

func (s *OwnedJobsSource) Run(ctx context.Context, sc *SourceContext) {
	s.mu.Lock()
	s.owned = map[string]ownedJob{}
	s.mu.Unlock()

	if sc.watcher.mode == WatchPoll {
		s.poll(ctx, sc)
		return
	}
	topics := map[api.Topic][]string{
		api.TopicJob:        {"*"},
		api.TopicAllocation: {"*"},
		api.TopicEvaluation: {"*"},
		api.TopicDeployment: {"*"},
	}
	sc.Watch(ctx, s.Name(), topics, func(ctx context.Context) (uint64, error) {
		_, index, err := s.list(ctx, sc, 0)
		return index, err
	}, func(_ context.Context, ev *api.Event) error {
		s.apply(ev, sc)
		return nil
	})
}

// list rebuilds the index of owned jobs and returns the keys of owners
// whose jobs changed or disappeared since the previous listing.
func (s *OwnedJobsSource) list(ctx context.Context, sc *SourceContext, waitIndex uint64) ([]ObjectKey, uint64, error) {
	stubs, index, err := sc.backend.ListJobs(ctx, sc.ns, waitIndex)
	if err != nil {
		return nil, 0, err
	}
	owned := map[string]ownedJob{}
	for _, stub := range stubs {
		if owner, ok := OwnerFromMeta(sc.kind, stub.Meta); ok {
			owned[jobKey(stub.Namespace, stub.ID)] = ownedJob{owner: owner, modifyIndex: stub.ModifyIndex}
		}
	}

	s.mu.Lock()
	var changed []ObjectKey
	for k, cur := range owned {
		if prev, ok := s.owned[k]; !ok || prev.modifyIndex != cur.modifyIndex {
			changed = append(changed, cur.owner)
		}
	}
	for k, prev := range s.owned {
		if _, ok := owned[k]; !ok {
			changed = append(changed, prev.owner)
		}
	}
	s.owned = owned
	s.mu.Unlock()
	return changed, index, nil
}

func (s *OwnedJobsSource) poll(ctx context.Context, sc *SourceContext) {
	var index uint64
	failures := 0
	for ctx.Err() == nil {
		changed, next, err := s.list(ctx, sc, index)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			sc.watcher.logger.Warn("polling owned jobs failed", "error", err)
			sleepCtx(ctx, sc.watcher.backoff(failures))
			continue
		}
		failures = 0
		if index == 0 {
			sc.EnqueueAll()
		} else {
			for _, key := range changed {
				sc.Enqueue(key)
			}
		}
		index = next
	}
}

func (s *OwnedJobsSource) lookup(namespace, jobID string) (ObjectKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.owned[jobKey(namespace, jobID)]
	return o.owner, ok
}

func (s *OwnedJobsSource) apply(ev *api.Event, sc *SourceContext) {
	switch ev.Topic {
	case api.TopicJob:
		job, err := ev.Job()
		if err != nil || job == nil || job.ID == nil {
			return
		}
		ns := ev.Namespace
		if job.Namespace != nil {
			ns = *job.Namespace
		}
		owner, ok := OwnerFromMeta(sc.kind, job.Meta)
		s.mu.Lock()
		if ok {
			var modifyIndex uint64
			if job.ModifyIndex != nil {
				modifyIndex = *job.ModifyIndex
			}
			s.owned[jobKey(ns, *job.ID)] = ownedJob{owner: owner, modifyIndex: modifyIndex}
		} else if prev, had := s.owned[jobKey(ns, *job.ID)]; had {
			// The owner reference was removed: the previous owner has
			// to notice it lost the job.
			delete(s.owned, jobKey(ns, *job.ID))
			owner, ok = prev.owner, true
		}
		s.mu.Unlock()
		if ok {
			sc.Enqueue(owner)
		}
	case api.TopicAllocation:
		alloc, err := ev.Allocation()
		if err != nil || alloc == nil {
			return
		}
		if owner, ok := s.lookup(alloc.Namespace, alloc.JobID); ok {
			sc.Enqueue(owner)
		}
	case api.TopicEvaluation:
		eval, err := ev.Evaluation()
		if err != nil || eval == nil {
			return
		}
		if owner, ok := s.lookup(eval.Namespace, eval.JobID); ok {
			sc.Enqueue(owner)
		}
	case api.TopicDeployment:
		d, err := ev.Deployment()
		if err != nil || d == nil {
			return
		}
		if owner, ok := s.lookup(d.Namespace, d.JobID); ok {
			sc.Enqueue(owner)
		}
	}
}
