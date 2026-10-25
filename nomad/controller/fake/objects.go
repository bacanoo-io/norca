// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
)

// --- Native objects ---

func objKey(ns string, kind controller.Kind, name string) string {
	return key(ns, kind.String()+"/"+name)
}

func copyObject(o *api.CustomObject) *api.CustomObject {
	raw, _ := json.Marshal(o)
	var out api.CustomObject
	_ = json.Unmarshal(raw, &out)
	return &out
}

// normalize converts a document to what a JSON round trip produces, as the
// server stores it.
func normalize(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (b *Backend) ListObjects(ctx context.Context, kind controller.Kind, namespace string, waitIndex uint64) ([]*api.CustomObject, uint64, error) {
	b.mu.Lock()
	if err := b.callLocked("ListObjects"); err != nil {
		b.mu.Unlock()
		return nil, 0, err
	}
	for waitIndex > 0 && b.objsIdx <= waitIndex {
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-ch:
		case <-time.After(5 * time.Second):
			return b.ListObjects(ctx, kind, namespace, 0)
		}
		b.mu.Lock()
	}
	defer b.mu.Unlock()
	var out []*api.CustomObject
	for _, o := range b.objs {
		if o.Group == kind.Group && o.Kind == kind.Kind && (namespace == "*" || o.Namespace == defaultNS(namespace)) {
			out = append(out, copyObject(o))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out, b.objsIdx, nil
}

func (b *Backend) ReadObject(_ context.Context, kind controller.Kind, namespace, name string) (*api.CustomObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("ReadObject"); err != nil {
		return nil, err
	}
	o, ok := b.objs[objKey(defaultNS(namespace), kind, name)]
	if !ok {
		return nil, fmt.Errorf("%s %s: %w", kind, name, controller.ErrNotFound)
	}
	return copyObject(o), nil
}

// GetObject returns a stored object or nil, without recording a call.
func (b *Backend) GetObject(kind controller.Kind, namespace, name string) *api.CustomObject {
	b.mu.Lock()
	defer b.mu.Unlock()
	if o, ok := b.objs[objKey(defaultNS(namespace), kind, name)]; ok {
		return copyObject(o)
	}
	return nil
}

func (b *Backend) ApplyObject(_ context.Context, obj *api.CustomObject, cas uint64) (*api.CustomObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("ApplyObject"); err != nil {
		return nil, err
	}
	return b.applyObjectLocked(obj, &cas)
}

// PutObject writes the spec and metadata of an object without
// check-and-set, like a user running "norca object apply".
func (b *Backend) PutObject(obj *api.CustomObject) *api.CustomObject {
	b.mu.Lock()
	defer b.mu.Unlock()
	out, err := b.applyObjectLocked(obj, nil)
	if err != nil {
		panic(err)
	}
	return out
}

func (b *Backend) applyObjectLocked(obj *api.CustomObject, cas *uint64) (*api.CustomObject, error) {
	ns := defaultNS(obj.Namespace)
	kind := controller.Kind{Group: obj.Group, Kind: obj.Kind}
	k := objKey(ns, kind, obj.Name)
	cur, exists := b.objs[k]
	if cas != nil && ((*cas == 0 && exists) || (*cas != 0 && (!exists || cur.ModifyIndex != *cas))) {
		return nil, fmt.Errorf("%w: %s %s", controller.ErrConflict, kind, obj.Name)
	}

	out := copyObject(obj)
	out.Namespace = ns
	out.Spec = normalize(out.Spec)
	// Like the server, a write adds its finalizers and never removes any.
	out.Finalizers = nil
	out.DeletionTime, out.DeletionPropagation = 0, ""
	if exists {
		out.Finalizers = slices.Clone(cur.Finalizers)
		out.DeletionTime, out.DeletionPropagation = cur.DeletionTime, cur.DeletionPropagation
	}
	for _, f := range obj.Finalizers {
		if !slices.Contains(out.Finalizers, f) {
			if out.DeletionTime != 0 {
				return nil, fmt.Errorf("%s %s is being deleted: %w", kind, obj.Name, controller.ErrConflict)
			}
			out.Finalizers = append(out.Finalizers, f)
		}
	}
	now := time.Now().UnixNano()
	if exists {
		out.ID = cur.ID
		out.Status = cur.Status
		out.Generation = cur.Generation
		out.CreateIndex, out.CreateTime = cur.CreateIndex, cur.CreateTime
		if !reflect.DeepEqual(cur.Spec, out.Spec) {
			out.Generation++
		}
		if out.Generation == cur.Generation && out.ManagedBy == cur.ManagedBy && maps.Equal(cur.Labels, out.Labels) &&
			slices.Equal(cur.Finalizers, out.Finalizers) && reflect.DeepEqual(cur.Owners, out.Owners) {
			// Nothing changed: nothing is written.
			return copyObject(cur), nil
		}
	} else {
		out.ID = fmt.Sprintf("id-%d", b.index+1)
		out.Status = nil
		out.Generation = 1
	}
	idx := b.nextIndexLocked()
	out.ModifyIndex, out.ModifyTime = idx, now
	if !exists {
		out.CreateIndex, out.CreateTime = idx, now
	}
	b.storeObjectLocked(k, out, api.CustomObjectUpserted)
	return copyObject(out), nil
}

func (b *Backend) storeObjectLocked(k string, o *api.CustomObject, eventType string) {
	b.objs[k] = o
	b.objsIdx = b.index
	b.publishLocked(api.Event{
		Topic: api.TopicCustomObject, Type: eventType, Key: o.KindRef() + "/" + o.Name, Namespace: o.Namespace,
		FilterKeys: []string{o.KindRef()},
		Payload:    toPayload("Object", o),
	})
	b.notifyLocked()
}

func (b *Backend) UpdateObjectStatus(_ context.Context, kind controller.Kind, namespace, name string, status map[string]any, cas uint64) (*api.CustomObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("UpdateObjectStatus"); err != nil {
		return nil, err
	}
	ns := defaultNS(namespace)
	k := objKey(ns, kind, name)
	cur, ok := b.objs[k]
	if !ok {
		return nil, fmt.Errorf("%s %s: %w", kind, name, controller.ErrNotFound)
	}
	if cas != 0 && cur.ModifyIndex != cas {
		return nil, fmt.Errorf("%w: %s %s", controller.ErrConflict, kind, name)
	}
	status = normalize(status)
	if reflect.DeepEqual(cur.Status, status) {
		return copyObject(cur), nil
	}
	out := copyObject(cur)
	out.Status = status
	out.ModifyIndex, out.ModifyTime = b.nextIndexLocked(), time.Now().UnixNano()
	b.storeObjectLocked(k, out, api.CustomObjectStatusUpdated)
	return copyObject(out), nil
}

func (b *Backend) DeleteObject(_ context.Context, kind controller.Kind, namespace, name string, cas uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("DeleteObject"); err != nil {
		return err
	}
	ns := defaultNS(namespace)
	cur, ok := b.objs[objKey(ns, kind, name)]
	if !ok {
		return nil
	}
	if cas != 0 && cur.ModifyIndex != cas {
		return fmt.Errorf("%w: %s %s", controller.ErrConflict, kind, name)
	}
	b.deleteObjectLocked(ns, kind, name)
	return nil
}

// RemoveObject deletes an object unconditionally, like a user running
// "norca object delete": an object with finalizers is only marked for
// deletion.
func (b *Backend) RemoveObject(kind controller.Kind, namespace, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteObjectLocked(defaultNS(namespace), kind, name)
}

func (b *Backend) deleteObjectLocked(ns string, kind controller.Kind, name string) {
	k := objKey(ns, kind, name)
	cur, ok := b.objs[k]
	if !ok {
		return
	}
	if len(cur.Finalizers) == 0 {
		b.removeObjectLocked(ns, kind, name)
		return
	}
	if cur.DeletionTime != 0 {
		return
	}
	out := copyObject(cur)
	out.DeletionTime = time.Now().UnixNano()
	out.DeletionPropagation = api.ObjectPropagationBackground
	out.ModifyIndex, out.ModifyTime = b.nextIndexLocked(), out.DeletionTime
	b.storeObjectLocked(k, out, api.CustomObjectUpserted)
}

func (b *Backend) UpdateObjectFinalizers(_ context.Context, kind controller.Kind, namespace, name string, add, remove []string, cas uint64) (*api.CustomObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("UpdateObjectFinalizers"); err != nil {
		return nil, err
	}
	ns := defaultNS(namespace)
	k := objKey(ns, kind, name)
	cur, ok := b.objs[k]
	if !ok {
		return nil, fmt.Errorf("%s %s: %w", kind, name, controller.ErrNotFound)
	}
	if cas != 0 && cur.ModifyIndex != cas {
		return nil, fmt.Errorf("%w: %s %s", controller.ErrConflict, kind, name)
	}
	out := copyObject(cur)
	out.Finalizers = slices.DeleteFunc(out.Finalizers, func(f string) bool { return slices.Contains(remove, f) })
	for _, f := range add {
		if slices.Contains(out.Finalizers, f) {
			continue
		}
		if out.DeletionTime != 0 {
			return nil, fmt.Errorf("%s %s is being deleted: %w", kind, name, controller.ErrConflict)
		}
		out.Finalizers = append(out.Finalizers, f)
	}
	if len(out.Finalizers) == 0 {
		out.Finalizers = nil
	}
	if slices.Equal(cur.Finalizers, out.Finalizers) {
		return copyObject(cur), nil
	}
	out.ModifyIndex, out.ModifyTime = b.nextIndexLocked(), time.Now().UnixNano()
	b.storeObjectLocked(k, out, api.CustomObjectUpserted)
	if out.DeletionTime != 0 && len(out.Finalizers) == 0 {
		b.removeObjectLocked(ns, kind, name)
	}
	return copyObject(out), nil
}

func (b *Backend) RecordObjectEvents(_ context.Context, kind controller.Kind, namespace, name string, events []*api.ObjectEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("RecordObjectEvents"); err != nil {
		return err
	}
	k := objKey(defaultNS(namespace), kind, name)
	if _, ok := b.objs[k]; !ok {
		return fmt.Errorf("%s %s: %w", kind, name, controller.ErrNotFound)
	}
	now := time.Now().UnixNano()
	log := b.objEvents[k]
	for _, e := range events {
		ev := *e
		if ev.Type == "" {
			ev.Type = api.ObjectEventTypeNormal
		}
		if n := len(log); n > 0 && log[n-1].Type == ev.Type && log[n-1].Reason == ev.Reason && log[n-1].Message == ev.Message {
			log[n-1].Count++
			log[n-1].LastTime = now
			continue
		}
		ev.Count, ev.FirstTime, ev.LastTime = 1, now, now
		log = append(log, &ev)
	}
	if len(log) > 20 {
		log = log[len(log)-20:]
	}
	b.objEvents[k] = log
	return nil
}

func (b *Backend) ListObjectEvents(_ context.Context, kind controller.Kind, namespace, name string) ([]*api.ObjectEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.callLocked("ListObjectEvents"); err != nil {
		return nil, err
	}
	var out []*api.ObjectEvent
	for _, e := range b.objEvents[objKey(defaultNS(namespace), kind, name)] {
		ev := *e
		out = append(out, &ev)
	}
	return out, nil
}

// ObjectEvents returns the event log of an object, without recording a
// call.
func (b *Backend) ObjectEvents(kind controller.Kind, namespace, name string) []*api.ObjectEvent {
	out, _ := b.ListObjectEvents(context.Background(), kind, namespace, name)
	return out
}

func (b *Backend) removeObjectLocked(ns string, kind controller.Kind, name string) {
	k := objKey(ns, kind, name)
	cur, ok := b.objs[k]
	if !ok {
		return
	}
	delete(b.objs, k)
	delete(b.objEvents, k)
	b.nextIndexLocked()
	b.objsIdx = b.index
	b.publishLocked(api.Event{
		Topic: api.TopicCustomObject, Type: api.CustomObjectDeleted, Key: cur.KindRef() + "/" + name, Namespace: ns,
		FilterKeys: []string{cur.KindRef()},
		Payload:    toPayload("Object", cur),
	})
	b.notifyLocked()
}
