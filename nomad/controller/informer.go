// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
)

// EventHandler receives the changes of the objects in an informer's cache.
// Handlers are called synchronously from the watch goroutine, so they must
// not block; typically they only add keys to a queue. Unset functions are
// skipped. The objects must not be modified.
type EventHandler struct {
	OnAdd    func(obj *Object)
	OnUpdate func(oldObj, newObj *Object)
	OnDelete func(obj *Object)
}

// InformerOptions configures an Informer.
type InformerOptions struct {
	// Namespace to watch; "*" watches every namespace the token can access.
	// Defaults to "default".
	Namespace string

	// ResyncPeriod forces a full listing periodically; 0 disables it.
	ResyncPeriod time.Duration

	// WatchMode selects the event stream or blocking queries.
	WatchMode WatchMode

	// Name identifies the informer in logs and metrics; defaults to the kind.
	Name    string
	Logger  *slog.Logger
	Metrics Metrics
}

// Informer keeps a local cache of the objects of a kind: it lists them, then
// follows the Variables event topic with exact resumption, and lists again
// whenever the event stream cannot be resumed (410 Gone).
//
// Objects whose Variable cannot be decoded (for example an invalid JSON
// spec) are logged and ignored: the cache keeps their last valid version.
type Informer struct {
	kind   Kind
	vars   VariablesAPI
	events EventsAPI
	opts   InformerOptions
	logger *slog.Logger

	mu       sync.RWMutex
	cache    map[ObjectKey]*Object
	handlers []EventHandler

	syncOnce sync.Once
	synced   chan struct{}
}

// NewInformer returns an informer for the objects of kind.
func NewInformer(kind Kind, vars VariablesAPI, events EventsAPI, opts InformerOptions) (*Informer, error) {
	if err := kind.Validate(); err != nil {
		return nil, err
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.Name == "" {
		opts.Name = kind.String()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = NopMetrics{}
	}
	return &Informer{
		kind:   kind,
		vars:   vars,
		events: events,
		opts:   opts,
		logger: opts.Logger.With("kind", kind.String()),
		cache:  map[ObjectKey]*Object{},
		synced: make(chan struct{}),
	}, nil
}

// Kind returns the kind of the informer.
func (i *Informer) Kind() Kind { return i.kind }

// AddHandler registers an event handler. Handlers should be added before Run.
func (i *Informer) AddHandler(h EventHandler) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.handlers = append(i.handlers, h)
}

// Run lists and watches until ctx is canceled.
func (i *Informer) Run(ctx context.Context) {
	w := &watcher{
		events:     i.events,
		mode:       i.opts.WatchMode,
		logger:     i.logger,
		metrics:    i.opts.Metrics,
		controller: i.opts.Name,
	}
	w.run(ctx, watchConfig{
		name:         "objects",
		namespace:    i.opts.Namespace,
		topics:       map[api.Topic][]string{api.TopicVariables: {i.kind.WatchPattern()}},
		list:         func(ctx context.Context) (uint64, error) { return i.sync(ctx, 0) },
		poll:         i.sync,
		apply:        i.apply,
		resyncPeriod: i.opts.ResyncPeriod,
	})
}

// HasSynced reports whether the first listing finished.
func (i *Informer) HasSynced() bool {
	select {
	case <-i.synced:
		return true
	default:
		return false
	}
}

// WaitForSync blocks until the first listing finished or ctx is canceled.
func (i *Informer) WaitForSync(ctx context.Context) bool {
	select {
	case <-i.synced:
		return true
	case <-ctx.Done():
		return false
	}
}

// Get returns a copy of the cached object.
func (i *Informer) Get(key ObjectKey) (*Object, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	obj, ok := i.cache[key]
	return obj.Copy(), ok
}

// List returns copies of the cached objects, sorted by key.
func (i *Informer) List() []*Object {
	i.mu.RLock()
	out := make([]*Object, 0, len(i.cache))
	for _, obj := range i.cache {
		out = append(out, obj.Copy())
	}
	i.mu.RUnlock()
	sort.Slice(out, func(a, b int) bool { return out[a].Key.String() < out[b].Key.String() })
	return out
}

// Keys returns the keys of the cached objects.
func (i *Informer) Keys() []ObjectKey {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]ObjectKey, 0, len(i.cache))
	for key := range i.cache {
		out = append(out, key)
	}
	return out
}

// sync lists the objects (with a blocking query after waitIndex if it is
// positive), updates the cache and calls the handlers for the differences.
func (i *Informer) sync(ctx context.Context, waitIndex uint64) (uint64, error) {
	metas, index, err := i.vars.ListVariables(ctx, i.opts.Namespace, i.kind.Prefix(), waitIndex)
	if err != nil {
		return 0, err
	}

	seen := make(map[ObjectKey]struct{}, len(metas))
	for _, meta := range metas {
		name, ok := i.kind.NameFromPath(meta.Path)
		if !ok {
			continue
		}
		key := ObjectKey{Namespace: meta.Namespace, Name: name}
		seen[key] = struct{}{}

		i.mu.RLock()
		cached := i.cache[key]
		i.mu.RUnlock()
		if cached != nil && cached.ModifyIndex == meta.ModifyIndex {
			continue
		}
		if err := i.fetch(ctx, key); err != nil {
			return 0, err
		}
	}

	i.mu.Lock()
	var deleted []*Object
	for key, obj := range i.cache {
		if _, ok := seen[key]; !ok {
			delete(i.cache, key)
			deleted = append(deleted, obj)
		}
	}
	handlers := i.handlers
	i.mu.Unlock()
	for _, obj := range deleted {
		notifyDelete(handlers, obj)
	}

	i.syncOnce.Do(func() { close(i.synced) })
	return index, nil
}

// fetch reads an object and stores it in the cache. A missing object is
// removed from the cache.
func (i *Informer) fetch(ctx context.Context, key ObjectKey) error {
	v, err := i.vars.ReadVariable(ctx, key.Namespace, i.kind.Path(key.Name))
	if errors.Is(err, ErrNotFound) {
		i.remove(key, 0)
		return nil
	}
	if err != nil {
		return err
	}
	obj, err := ObjectFromVariable(i.kind, v)
	if err != nil {
		i.logger.Warn("ignoring invalid object", "object", key.String(), "error", err)
		return nil
	}
	i.store(obj)
	return nil
}

func (i *Informer) store(obj *Object) {
	i.mu.Lock()
	old := i.cache[obj.Key]
	if old != nil && old.ModifyIndex >= obj.ModifyIndex {
		i.mu.Unlock()
		return
	}
	i.cache[obj.Key] = obj
	handlers := i.handlers
	i.mu.Unlock()

	for _, h := range handlers {
		switch {
		case old == nil && h.OnAdd != nil:
			h.OnAdd(obj)
		case old != nil && h.OnUpdate != nil:
			h.OnUpdate(old, obj)
		}
	}
}

// remove deletes the object from the cache unless it was modified after
// index (0: unconditionally).
func (i *Informer) remove(key ObjectKey, index uint64) {
	i.mu.Lock()
	old := i.cache[key]
	if old == nil || (index > 0 && old.ModifyIndex > index) {
		i.mu.Unlock()
		return
	}
	delete(i.cache, key)
	handlers := i.handlers
	i.mu.Unlock()
	notifyDelete(handlers, old)
}

func notifyDelete(handlers []EventHandler, obj *Object) {
	for _, h := range handlers {
		if h.OnDelete != nil {
			h.OnDelete(obj)
		}
	}
}

// apply processes one Variables event. Events carry only metadata, so the
// object is read for upserts.
func (i *Informer) apply(ctx context.Context, ev *api.Event) error {
	if ev.Topic != api.TopicVariables {
		return nil
	}
	meta, err := ev.Variable()
	if err != nil || meta == nil {
		i.logger.Warn("invalid Variables event", "key", ev.Key, "error", err)
		return nil
	}
	name, ok := i.kind.NameFromPath(meta.Path)
	if !ok {
		return nil
	}
	key := ObjectKey{Namespace: meta.Namespace, Name: name}

	if ev.Type == "VariableDeleted" {
		i.remove(key, ev.Index)
		return nil
	}
	i.mu.RLock()
	cached := i.cache[key]
	i.mu.RUnlock()
	if cached != nil && cached.ModifyIndex >= meta.ModifyIndex {
		return nil
	}
	return i.fetch(ctx, key)
}
