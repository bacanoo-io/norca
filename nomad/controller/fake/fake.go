// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package fake provides an in-memory implementation of controller.Backend
// for unit tests of controllers: Variables with check-and-set and blocking
// queries, native custom objects (with a server maintained generation and a
// separate status, but without schema validation or default values), jobs,
// and an event stream with exact resumption whose buffer can be truncated to
// provoke 410 Gone.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
)

// Backend is an in-memory controller.Backend. The zero value is not usable;
// use New.
type Backend struct {
	mu    sync.Mutex
	index uint64
	vars  map[string]*api.Variable // namespace + "\x00" + path
	jobs  map[string]*api.Job      // namespace + "\x00" + id
	objs  map[string]*api.CustomObject
	// objEvents are the event logs of the objects.
	objEvents map[string][]*api.ObjectEvent
	varsIdx   uint64
	jobsIdx   uint64
	objsIdx   uint64

	events     []api.Event
	oldest     uint64 // events below this index were dropped
	changed    chan struct{}
	streams    map[*stream]struct{}
	forbidden  bool
	failNext   map[string]error
	callCounts map[string]int
}

type stream struct {
	cancel context.CancelFunc
	kill   chan struct{}
}

// New returns an empty backend.
func New() *Backend {
	return &Backend{
		vars:       map[string]*api.Variable{},
		jobs:       map[string]*api.Job{},
		objs:       map[string]*api.CustomObject{},
		objEvents:  map[string][]*api.ObjectEvent{},
		changed:    make(chan struct{}),
		streams:    map[*stream]struct{}{},
		failNext:   map[string]error{},
		callCounts: map[string]int{},
		index:      10,
		oldest:     1,
	}
}

var _ controller.Backend = (*Backend)(nil)

func key(ns, id string) string { return ns + "\x00" + id }

func defaultNS(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

// notifyLocked wakes up blocking queries and streams.
func (b *Backend) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *Backend) nextIndexLocked() uint64 {
	b.index++
	return b.index
}

// call records a call and returns an injected error, if any.
func (b *Backend) callLocked(op string) error {
	b.callCounts[op]++
	if err, ok := b.failNext[op]; ok {
		delete(b.failNext, op)
		return err
	}
	return nil
}

// FailNext makes the next call of op (the method name, e.g. "ReadVariable")
// fail with err.
func (b *Backend) FailNext(op string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failNext[op] = err
}

// Calls returns how many times op was called.
func (b *Backend) Calls(op string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.callCounts[op]
}

// Index returns the latest index.
func (b *Backend) Index() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.index
}

func toPayload(name string, obj any) map[string]any {
	raw, _ := json.Marshal(map[string]any{name: obj})
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (b *Backend) publishLocked(ev api.Event) {
	ev.Index = b.index
	b.events = append(b.events, ev)
}

// Publish appends an arbitrary event (for example a Service event) at a new
// index.
func (b *Backend) Publish(ev api.Event) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextIndexLocked()
	b.publishLocked(ev)
	b.notifyLocked()
	return b.index
}

// TruncateEvents drops the buffered events below index, so exact streams
// from an older index get 410 Gone. Active streams are interrupted.
func (b *Backend) TruncateEvents(index uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.events[:0]
	for _, ev := range b.events {
		if ev.Index >= index {
			kept = append(kept, ev)
		}
	}
	b.events = kept
	b.oldest = index
	b.dropStreamsLocked()
}

// DropStreams interrupts the active event streams, like a server restart.
func (b *Backend) DropStreams() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropStreamsLocked()
}

func (b *Backend) dropStreamsLocked() {
	for s := range b.streams {
		close(s.kill)
		delete(b.streams, s)
	}
}

// SetStreamForbidden makes Stream fail with 403, like the event stream with
// a token that may not subscribe to the topics.
func (b *Backend) SetStreamForbidden(forbidden bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.forbidden = forbidden
}

// --- Variables ---

func (b *Backend) ListVariables(ctx context.Context, namespace, prefix string, waitIndex uint64) ([]*api.VariableMetadata, uint64, error) {
	b.mu.Lock()
	if err := b.callLocked("ListVariables"); err != nil {
		b.mu.Unlock()
		return nil, 0, err
	}
	for waitIndex > 0 && b.varsIdx <= waitIndex {
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-ch:
		case <-time.After(5 * time.Second):
			b.mu.Lock()
			idx := b.varsIdx
			b.mu.Unlock()
			out, _, err := b.ListVariables(ctx, namespace, prefix, 0)
			return out, idx, err
		}
		b.mu.Lock()
	}
	defer b.mu.Unlock()
	var out []*api.VariableMetadata
	for _, v := range b.vars {
		if (namespace == "*" || v.Namespace == defaultNS(namespace)) && strings.HasPrefix(v.Path, prefix) {
			out = append(out, v.Metadata())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, b.varsIdx, nil
}

func (b *Backend) ReadVariable(_ context.Context, namespace, path string) (*api.Variable, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("ReadVariable"); err != nil {
		return nil, err
	}
	v, ok := b.vars[key(defaultNS(namespace), path)]
	if !ok {
		return nil, fmt.Errorf("variable %s: %w", path, controller.ErrNotFound)
	}
	return v.Copy(), nil
}

// GetVariable returns a stored Variable or nil, without recording a call.
func (b *Backend) GetVariable(namespace, path string) *api.Variable {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.vars[key(defaultNS(namespace), path)]; ok {
		return v.Copy()
	}
	return nil
}

func (b *Backend) WriteVariable(_ context.Context, v *api.Variable) (*api.Variable, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("WriteVariable"); err != nil {
		return nil, err
	}
	return b.putVariableLocked(v, true)
}

// PutVariable writes a Variable without check-and-set, like a user running
// "var put".
func (b *Backend) PutVariable(v *api.Variable) *api.Variable {
	b.mu.Lock()
	defer b.mu.Unlock()
	out, _ := b.putVariableLocked(v, false)
	return out
}

func (b *Backend) putVariableLocked(v *api.Variable, cas bool) (*api.Variable, error) {
	ns := defaultNS(v.Namespace)
	k := key(ns, v.Path)
	cur, exists := b.vars[k]
	if cas {
		if (v.ModifyIndex == 0 && exists) || (v.ModifyIndex != 0 && (!exists || cur.ModifyIndex != v.ModifyIndex)) {
			return nil, fmt.Errorf("%w: variable %s", controller.ErrConflict, v.Path)
		}
	}
	if len(v.Items) == 0 {
		return nil, errors.New("variable missing items")
	}
	idx := b.nextIndexLocked()
	now := time.Now().UnixNano()
	out := v.Copy()
	out.Namespace = ns
	out.ModifyIndex = idx
	out.ModifyTime = now
	if exists {
		out.CreateIndex, out.CreateTime = cur.CreateIndex, cur.CreateTime
	} else {
		out.CreateIndex, out.CreateTime = idx, now
	}
	b.vars[k] = out
	b.varsIdx = idx
	b.publishLocked(api.Event{
		Topic: api.TopicVariables, Type: "VariableUpserted", Key: out.Path, Namespace: ns,
		Payload: toPayload("Variable", out.Metadata()),
	})
	b.notifyLocked()
	return out.Copy(), nil
}

func (b *Backend) DeleteVariable(_ context.Context, namespace, path string, modifyIndex uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("DeleteVariable"); err != nil {
		return err
	}
	ns := defaultNS(namespace)
	cur, ok := b.vars[key(ns, path)]
	if !ok {
		return nil
	}
	if modifyIndex != 0 && cur.ModifyIndex != modifyIndex {
		return fmt.Errorf("%w: variable %s", controller.ErrConflict, path)
	}
	b.removeVariableLocked(ns, path)
	return nil
}

// RemoveVariable deletes a Variable unconditionally, like a user running
// "var purge".
func (b *Backend) RemoveVariable(namespace, path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeVariableLocked(defaultNS(namespace), path)
}

func (b *Backend) removeVariableLocked(ns, path string) {
	cur, ok := b.vars[key(ns, path)]
	if !ok {
		return
	}
	delete(b.vars, key(ns, path))
	idx := b.nextIndexLocked()
	b.varsIdx = idx
	b.publishLocked(api.Event{
		Topic: api.TopicVariables, Type: "VariableDeleted", Key: path, Namespace: ns,
		Payload: toPayload("Variable", cur.Metadata()),
	})
	b.notifyLocked()
}

// --- Events ---

func matches(ev api.Event, namespace string, topics map[api.Topic][]string) bool {
	if namespace != "*" && ev.Namespace != "" && ev.Namespace != defaultNS(namespace) {
		return false
	}
	keys := append(topics[api.TopicAll], topics[ev.Topic]...)
	for _, k := range keys {
		switch {
		case k == "*" || k == ev.Key || slices.Contains(ev.FilterKeys, k):
			return true
		case strings.HasSuffix(k, "*") && strings.HasPrefix(ev.Key, strings.TrimSuffix(k, "*")):
			return true
		}
	}
	return false
}

func (b *Backend) Stream(ctx context.Context, namespace string, topics map[api.Topic][]string, index uint64, exact bool) (<-chan *api.Events, error) {
	b.mu.Lock()
	if err := b.callLocked("Stream"); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if b.forbidden {
		b.mu.Unlock()
		return nil, forbiddenError()
	}
	if exact && index < b.oldest {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: requested %d, oldest %d", api.ErrEventIndexGone, index, b.oldest)
	}
	if !exact && (index == 0 || index < b.oldest) {
		index = b.index + 1
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{cancel: cancel, kill: make(chan struct{})}
	b.streams[s] = struct{}{}
	b.mu.Unlock()

	out := make(chan *api.Events, 10)
	go func() {
		defer close(out)
		defer cancel()
		next := index
		for {
			b.mu.Lock()
			var batch []*api.Events
			for _, ev := range b.events {
				if ev.Index < next || !matches(ev, namespace, topics) {
					continue
				}
				if n := len(batch); n > 0 && batch[n-1].Index == ev.Index {
					batch[n-1].Events = append(batch[n-1].Events, ev)
				} else {
					batch = append(batch, &api.Events{Index: ev.Index, Events: []api.Event{ev}})
				}
			}
			if b.index >= next {
				next = b.index + 1
			}
			ch := b.changed
			b.mu.Unlock()

			for _, evs := range batch {
				select {
				case out <- evs:
				case <-ctx.Done():
					return
				case <-s.kill:
					b.sendErr(ctx, out)
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-s.kill:
				b.sendErr(ctx, out)
				return
			case <-ch:
			}
		}
	}()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.streams, s)
		b.mu.Unlock()
	}()
	return out, nil
}

func (b *Backend) sendErr(ctx context.Context, out chan<- *api.Events) {
	select {
	case out <- &api.Events{Err: errors.New("stream closed")}:
	case <-ctx.Done():
	}
}

// --- Jobs ---

func (b *Backend) ListJobs(ctx context.Context, namespace string, waitIndex uint64) ([]*api.JobListStub, uint64, error) {
	b.mu.Lock()
	if err := b.callLocked("ListJobs"); err != nil {
		b.mu.Unlock()
		return nil, 0, err
	}
	for waitIndex > 0 && b.jobsIdx <= waitIndex {
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-ch:
		}
		b.mu.Lock()
	}
	defer b.mu.Unlock()
	var out []*api.JobListStub
	for _, j := range b.jobs {
		if namespace != "*" && *j.Namespace != defaultNS(namespace) {
			continue
		}
		out = append(out, &api.JobListStub{
			ID: *j.ID, Name: *j.Name, Namespace: *j.Namespace, Type: *j.Type, Stop: *j.Stop,
			Status: *j.Status, CreateIndex: *j.CreateIndex, ModifyIndex: *j.ModifyIndex,
			JobModifyIndex: *j.JobModifyIndex, Meta: maps.Clone(j.Meta),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, b.jobsIdx, nil
}

func copyJob(j *api.Job) *api.Job {
	raw, _ := json.Marshal(j)
	var out api.Job
	_ = json.Unmarshal(raw, &out)
	return &out
}

func (b *Backend) ReadJob(_ context.Context, namespace, id string) (*api.Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("ReadJob"); err != nil {
		return nil, err
	}
	j, ok := b.jobs[key(defaultNS(namespace), id)]
	if !ok {
		return nil, fmt.Errorf("job %s: %w", id, controller.ErrNotFound)
	}
	return copyJob(j), nil
}

// GetJob returns a stored job or nil, without recording a call.
func (b *Backend) GetJob(namespace, id string) *api.Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	if j, ok := b.jobs[key(defaultNS(namespace), id)]; ok {
		return copyJob(j)
	}
	return nil
}

// Jobs returns the IDs of the stored jobs of a namespace.
func (b *Backend) Jobs(namespace string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, j := range b.jobs {
		if *j.Namespace == defaultNS(namespace) {
			out = append(out, *j.ID)
		}
	}
	sort.Strings(out)
	return out
}

func (b *Backend) RegisterJob(_ context.Context, job *api.Job) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("RegisterJob"); err != nil {
		return err
	}
	b.registerJobLocked(job)
	return nil
}

// PutJob registers a job without recording a call, like a user running
// "job run".
func (b *Backend) PutJob(job *api.Job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.registerJobLocked(job)
}

func (b *Backend) registerJobLocked(job *api.Job) {
	j := copyJob(job)
	if j.ID == nil {
		panic("job without ID")
	}
	ns := "default"
	if j.Namespace != nil {
		ns = defaultNS(*j.Namespace)
	}
	idx := b.nextIndexLocked()
	var version uint64
	cur, exists := b.jobs[key(ns, *j.ID)]
	createIndex := idx
	if exists {
		createIndex = *cur.CreateIndex
		version = *cur.Version + 1
	}
	j.Namespace = &ns
	if j.Name == nil {
		j.Name = j.ID
	}
	if j.Type == nil {
		t := "service"
		j.Type = &t
	}
	stop := j.Stop != nil && *j.Stop
	status := "running"
	if stop {
		status = "dead"
	}
	j.Stop, j.Status = &stop, &status
	j.CreateIndex, j.ModifyIndex, j.JobModifyIndex, j.Version = &createIndex, &idx, &idx, &version
	b.jobs[key(ns, *j.ID)] = j
	b.jobsIdx = idx
	b.publishLocked(api.Event{
		Topic: api.TopicJob, Type: "JobRegistered", Key: *j.ID, Namespace: ns,
		Payload: toPayload("Job", j),
	})
	b.notifyLocked()
}

func (b *Backend) DeregisterJob(_ context.Context, namespace, id string, purge bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("DeregisterJob"); err != nil {
		return err
	}
	b.deregisterJobLocked(defaultNS(namespace), id, purge)
	return nil
}

// RemoveJob deregisters a job without recording a call, like a user running
// "job stop".
func (b *Backend) RemoveJob(namespace, id string, purge bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deregisterJobLocked(defaultNS(namespace), id, purge)
}

func (b *Backend) deregisterJobLocked(ns, id string, purge bool) {
	j, ok := b.jobs[key(ns, id)]
	if !ok {
		return
	}
	idx := b.nextIndexLocked()
	b.jobsIdx = idx
	if purge {
		// Like Norca servers, purging a job publishes a Job event with the
		// last known job (Nomad 1.6.5 publishes only the evaluation).
		delete(b.jobs, key(ns, id))
	} else {
		stop, status := true, "dead"
		j.Stop, j.Status, j.ModifyIndex = &stop, &status, &idx
	}
	b.publishLocked(api.Event{
		Topic: api.TopicJob, Type: "JobDeregistered", Key: id, Namespace: ns,
		Payload: toPayload("Job", j),
	})
	b.publishLocked(api.Event{
		Topic: api.TopicEvaluation, Type: "EvaluationUpdated", Key: fmt.Sprintf("eval-%d", idx), Namespace: ns,
		Payload: toPayload("Evaluation", &api.Evaluation{ID: fmt.Sprintf("eval-%d", idx), Namespace: ns, JobID: id}),
	})
	b.notifyLocked()
}

// forbiddenError returns an error that looks like a 403 response of the
// API client.
func forbiddenError() error {
	return controller.NewStatusError(http.StatusForbidden, "Permission denied")
}
