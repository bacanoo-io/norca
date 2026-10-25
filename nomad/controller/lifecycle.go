// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
)

// Lifecycle helpers for native objects: finalizers, the object event log,
// owner references of objects and jobs, the managed_by identity of the
// controller, adoption and the count rule of the autoscaler.

// ErrUnsupported is returned by lifecycle helpers that need native objects
// when the objects are stored as Variables.
var ErrUnsupported = errors.New("not supported with Variables storage")

// Event types of the object event log.
const (
	EventTypeNormal  = api.ObjectEventTypeNormal
	EventTypeWarning = api.ObjectEventTypeWarning
)

func (c *Objects) nativeAPI() (ObjectsAPI, error) {
	s, ok := c.store.(*nativeStore)
	if !ok {
		return nil, ErrUnsupported
	}
	return s.objs, nil
}

// AddFinalizer adds a finalizer to the object, so that deleting it only
// marks it for deletion until the controller has cleaned up and removed
// the finalizer. It requires the "manage-finalizers" capability and fails
// with a conflict if the object is already being deleted. It returns the
// object as stored.
func (c *Objects) AddFinalizer(ctx context.Context, obj *Object, finalizer string) (*Object, error) {
	if obj.HasFinalizer(finalizer) {
		return obj, nil
	}
	return c.updateFinalizers(ctx, obj, []string{finalizer}, nil)
}

// RemoveFinalizer removes a finalizer of the object. Removing the last
// finalizer of an object marked for deletion deletes it. A missing object
// is not an error (it returns nil).
func (c *Objects) RemoveFinalizer(ctx context.Context, obj *Object, finalizer string) (*Object, error) {
	if !obj.HasFinalizer(finalizer) {
		return obj, nil
	}
	out, err := c.updateFinalizers(ctx, obj, nil, []string{finalizer})
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return out, err
}

func (c *Objects) updateFinalizers(ctx context.Context, obj *Object, add, remove []string) (*Object, error) {
	objs, err := c.nativeAPI()
	if err != nil {
		return nil, err
	}
	kind := c.store.kind()
	// Finalizer changes are set operations: no check-and-set is needed.
	co, err := objs.UpdateObjectFinalizers(ctx, kind, obj.Key.Namespace, obj.Key.Name, add, remove, 0)
	if err != nil {
		return nil, fmt.Errorf("updating the finalizers of %s %s: %w", kind, obj.Key, err)
	}
	return ObjectFromCustomObject(kind, co)
}

// RecordEvent adds an event to the event log of the object (the
// "write-events" capability). Repetitions of the newest event are counted
// by the server. With Variables storage it does nothing.
func (c *Objects) RecordEvent(ctx context.Context, key ObjectKey, eventType, reason, message string) error {
	objs, err := c.nativeAPI()
	if err != nil {
		return nil
	}
	return objs.RecordObjectEvents(ctx, c.store.kind(), key.Namespace, key.Name, []*api.ObjectEvent{
		{Type: eventType, Reason: reason, Message: message},
	})
}

// Events returns the event log of the object, oldest first.
func (c *Objects) Events(ctx context.Context, key ObjectKey) ([]*api.ObjectEvent, error) {
	objs, err := c.nativeAPI()
	if err != nil {
		return nil, err
	}
	return objs.ListObjectEvents(ctx, c.store.kind(), key.Namespace, key.Name)
}

// EventRecorder records object events and drops repetitions of the same
// event of an object within a window, so a controller reconciling in a loop
// does not turn every reconcile into a Raft write. Errors are logged, not
// returned: events are informational.
type EventRecorder struct {
	objects *Objects
	window  time.Duration
	now     func() time.Time

	mu   sync.Mutex
	last map[ObjectKey]recordedEvent
}

type recordedEvent struct {
	event string
	at    time.Time
}

// NewEventRecorder returns a recorder that drops repetitions within window
// (one minute if zero).
func NewEventRecorder(objects *Objects, window time.Duration) *EventRecorder {
	if window <= 0 {
		window = time.Minute
	}
	return &EventRecorder{objects: objects, window: window, now: time.Now, last: map[ObjectKey]recordedEvent{}}
}

// Event records an event, unless the newest event recorded for the object
// is the same and younger than the window.
func (r *EventRecorder) Event(ctx context.Context, key ObjectKey, eventType, reason, message string) {
	id := eventType + "\x00" + reason + "\x00" + message
	now := r.now()
	r.mu.Lock()
	if prev, ok := r.last[key]; ok && prev.event == id && now.Sub(prev.at) < r.window {
		r.mu.Unlock()
		return
	}
	r.last[key] = recordedEvent{event: id, at: now}
	r.mu.Unlock()

	err := r.objects.RecordEvent(ctx, key, eventType, reason, message)
	if err != nil {
		// Allow the next attempt to record it.
		r.mu.Lock()
		delete(r.last, key)
		r.mu.Unlock()
		if !errors.Is(err, ErrNotFound) {
			slog.Default().Warn("recording an object event failed", "object", key.String(), "reason", reason, "error", err)
		}
	}
}

// Forget drops the memory of an object, for example after it was deleted.
func (r *EventRecorder) Forget(key ObjectKey) {
	r.mu.Lock()
	delete(r.last, key)
	r.mu.Unlock()
}

// OwnerReference returns the reference to obj for the Owners of objects it
// owns.
func OwnerReference(obj *Object) *api.ObjectOwnerReference {
	return &api.ObjectOwnerReference{
		Kind:      obj.Kind.String(),
		Namespace: obj.Key.Namespace,
		Name:      obj.Key.Name,
		ID:        obj.ID,
	}
}

// SetObjectOwner adds owner to the owners of obj (once), so the server's
// garbage collector deletes obj when owner is deleted (except with the
// orphan deletion mode).
func SetObjectOwner(obj, owner *Object) {
	ref := OwnerReference(owner)
	for _, r := range obj.Owners {
		if r != nil && r.Kind == ref.Kind && r.Name == ref.Name && (r.Namespace == ref.Namespace || r.Namespace == "") {
			return
		}
	}
	obj.Owners = append(obj.Owners, ref)
}

// IsObjectOwnedBy reports whether obj lists owner among its owners.
func IsObjectOwnedBy(obj, owner *Object) bool {
	for _, r := range obj.Owners {
		if r != nil && r.Kind == owner.Kind.String() && r.Name == owner.Key.Name &&
			(r.Namespace == owner.Key.Namespace || r.Namespace == "") {
			return true
		}
	}
	return false
}

// ManagedByFromEnv returns the managed_by identity of a controller running
// as a Nomad task with its workload identity: "workload:<namespace>/<job>"
// from the NOMAD_NAMESPACE and NOMAD_JOB_ID variables of the task
// environment, or "" outside a task.
func ManagedByFromEnv() string {
	ns, job := os.Getenv("NOMAD_NAMESPACE"), os.Getenv("NOMAD_JOB_ID")
	if ns == "" || job == "" {
		return ""
	}
	return "workload:" + ns + "/" + job
}

// SetManagedBy marks the job as managed by the identity (see
// ManagedByFromEnv): the servers reject updates, stops and scaling of the
// job by other identities (management tokens excepted). An empty identity
// leaves the job unprotected.
func SetManagedBy(job *api.Job, identity string) {
	if identity == "" {
		return
	}
	if job.Meta == nil {
		job.Meta = map[string]string{}
	}
	job.Meta[api.JobMetaManagedBy] = identity
}

// AdoptJobs returns the jobs of the namespace that belong to obj: those
// whose owner reference names it, and, for adoption, jobs without an owner
// reference whose meta contains all the selector pairs, which are
// registered again with the owner reference of obj. Use it after a restore
// (or a deletion in the orphan mode followed by a re-creation of the
// object) so the controller takes over existing jobs instead of creating
// duplicates. Jobs owned by another object are left alone.
func AdoptJobs(ctx context.Context, jobs JobsAPI, namespace string, obj *Object, selector map[string]string) ([]*api.JobListStub, error) {
	stubs, _, err := jobs.ListJobs(ctx, namespace, 0)
	if err != nil {
		return nil, err
	}
	var out []*api.JobListStub
	for _, stub := range stubs {
		if IsOwnedBy(stub.Meta, obj) {
			out = append(out, stub)
			continue
		}
		if _, owned := stub.Meta[OwnerMetaKey]; owned || len(selector) == 0 || !metaMatches(stub.Meta, selector) {
			continue
		}
		job, err := jobs.ReadJob(ctx, stub.Namespace, stub.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		SetOwner(job, obj)
		if err := jobs.RegisterJob(ctx, job); err != nil {
			return nil, fmt.Errorf("adopting job %q: %w", stub.ID, err)
		}
		stub.Meta = job.Meta
		out = append(out, stub)
	}
	return out, nil
}

func metaMatches(meta, selector map[string]string) bool {
	for k, v := range selector {
		if got, ok := meta[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// PreserveScaledCounts applies the count rule of the autoscaler: a task
// group with its own enabled scaling block (scaling its group, not a custom
// object) is scaled by the autoscaler, so the controller must not reset
// its count. It copies the count of such groups from the existing job into
// the desired job before the controller registers it.
func PreserveScaledCounts(desired, existing *api.Job) {
	if desired == nil || existing == nil {
		return
	}
	counts := map[string]*int{}
	for _, tg := range existing.TaskGroups {
		if tg != nil && tg.Name != nil && tg.Count != nil {
			counts[*tg.Name] = tg.Count
		}
	}
	for _, tg := range desired.TaskGroups {
		if tg == nil || tg.Name == nil || !scalesItself(tg.Scaling) {
			continue
		}
		if count, ok := counts[*tg.Name]; ok {
			c := *count
			tg.Count = &c
		}
	}
}

// scalesItself reports whether a scaling block is enabled and targets its
// own task group.
func scalesItself(s *api.ScalingPolicy) bool {
	if s == nil || (s.Enabled != nil && !*s.Enabled) {
		return false
	}
	raw, ok := s.Policy["target"]
	if !ok {
		return true
	}
	return !strings.Contains(fmt.Sprint(raw), "custom-object")
}
