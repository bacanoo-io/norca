// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hashicorp/nomad/api"
)

// objectStore is the storage of the objects of one kind: Variables or
// native objects. Objects and Informer use it, so controllers work the same
// way with both.
type objectStore interface {
	storage() Storage
	kind() Kind

	// validateName checks that the name can be stored.
	validateName(name string) error

	// get reads an object; it returns an error wrapping ErrNotFound if it
	// does not exist.
	get(ctx context.Context, key ObjectKey) (*Object, error)

	// list lists the objects of the namespace ("*" for all), with a
	// blocking query after waitIndex if it is positive. Entries without an
	// object (stores whose listings carry only metadata) are read with get
	// when needed.
	list(ctx context.Context, namespace string, waitIndex uint64) ([]listEntry, uint64, error)

	// write writes the spec and metadata of the object with check-and-set
	// on its ModifyIndex (0: create).
	write(ctx context.Context, obj *Object) (*Object, error)

	// writeStatus writes the status of the object with check-and-set on
	// its ModifyIndex, keeping the rest of the stored object.
	writeStatus(ctx context.Context, obj *Object, status json.RawMessage) (*Object, error)

	// delete deletes the object with check-and-set on modifyIndex (0:
	// unconditionally). A missing object is not an error.
	delete(ctx context.Context, key ObjectKey, modifyIndex uint64) error

	// topics returns the event stream topics of the objects.
	topics() map[api.Topic][]string

	// decodeEvent returns the change described by an event of the
	// topics, or false if the event is not about an object of the kind.
	decodeEvent(ev *api.Event) (objectChange, bool)
}

// errInvalidObject is wrapped by the errors of get for stored objects that
// can't be decoded (for example a Variable with an invalid JSON spec).
var errInvalidObject = errors.New("invalid object")

// decoded wraps a decoding error in errInvalidObject.
func decoded(obj *Object, err error) (*Object, error) {
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidObject, err)
	}
	return obj, nil
}

// listEntry is an object of a listing.
type listEntry struct {
	key         ObjectKey
	modifyIndex uint64
	obj         *Object // nil: read it with get
}

// objectChange is the change described by an event.
type objectChange struct {
	key         ObjectKey
	modifyIndex uint64
	obj         *Object // nil: read it with get
	deleted     bool
}

// variablesStore stores objects as Variables (StorageVariables).
type variablesStore struct {
	k    Kind
	vars VariablesAPI
}

func (s *variablesStore) storage() Storage { return StorageVariables }
func (s *variablesStore) kind() Kind       { return s.k }

func (s *variablesStore) validateName(name string) error { return ValidateName(name) }

func (s *variablesStore) get(ctx context.Context, key ObjectKey) (*Object, error) {
	v, err := s.vars.ReadVariable(ctx, key.Namespace, s.k.Path(key.Name))
	if err != nil {
		return nil, err
	}
	return decoded(ObjectFromVariable(s.k, v))
}

func (s *variablesStore) list(ctx context.Context, namespace string, waitIndex uint64) ([]listEntry, uint64, error) {
	metas, index, err := s.vars.ListVariables(ctx, namespace, s.k.Prefix(), waitIndex)
	if err != nil {
		return nil, 0, err
	}
	out := make([]listEntry, 0, len(metas))
	for _, meta := range metas {
		name, ok := s.k.NameFromPath(meta.Path)
		if !ok {
			continue
		}
		out = append(out, listEntry{
			key:         ObjectKey{Namespace: meta.Namespace, Name: name},
			modifyIndex: meta.ModifyIndex,
		})
	}
	return out, index, nil
}

func (s *variablesStore) write(ctx context.Context, obj *Object) (*Object, error) {
	v, err := obj.ToVariable()
	if err != nil {
		return nil, err
	}
	out, err := s.vars.WriteVariable(ctx, v)
	if err != nil {
		return nil, err
	}
	return ObjectFromVariable(s.k, out)
}

func (s *variablesStore) writeStatus(ctx context.Context, obj *Object, status json.RawMessage) (*Object, error) {
	if obj.ModifyIndex == 0 {
		return nil, errors.New("a status update requires the ModifyIndex of the object")
	}
	next := obj.Copy()
	next.Status = status
	return s.write(ctx, next)
}

func (s *variablesStore) delete(ctx context.Context, key ObjectKey, modifyIndex uint64) error {
	return s.vars.DeleteVariable(ctx, key.Namespace, s.k.Path(key.Name), modifyIndex)
}

func (s *variablesStore) topics() map[api.Topic][]string {
	return map[api.Topic][]string{api.TopicVariables: {s.k.WatchPattern()}}
}

// decodeEvent decodes a Variables event. Events carry only metadata, so
// upserted objects are read.
func (s *variablesStore) decodeEvent(ev *api.Event) (objectChange, bool) {
	if ev.Topic != api.TopicVariables {
		return objectChange{}, false
	}
	meta, err := ev.Variable()
	if err != nil || meta == nil {
		return objectChange{}, false
	}
	name, ok := s.k.NameFromPath(meta.Path)
	if !ok {
		return objectChange{}, false
	}
	return objectChange{
		key:         ObjectKey{Namespace: meta.Namespace, Name: name},
		modifyIndex: meta.ModifyIndex,
		deleted:     ev.Type == "VariableDeleted",
	}, true
}

// nativeStore stores objects as native objects of a registered kind
// (StorageNative).
type nativeStore struct {
	k    Kind
	objs ObjectsAPI
}

func (s *nativeStore) storage() Storage { return StorageNative }
func (s *nativeStore) kind() Kind       { return s.k }

func (s *nativeStore) validateName(name string) error { return ValidateNativeName(name) }

func (s *nativeStore) get(ctx context.Context, key ObjectKey) (*Object, error) {
	co, err := s.objs.ReadObject(ctx, s.k, key.Namespace, key.Name)
	if err != nil {
		return nil, err
	}
	return decoded(ObjectFromCustomObject(s.k, co))
}

func (s *nativeStore) list(ctx context.Context, namespace string, waitIndex uint64) ([]listEntry, uint64, error) {
	objs, index, err := s.objs.ListObjects(ctx, s.k, namespace, waitIndex)
	if err != nil {
		return nil, 0, err
	}
	out := make([]listEntry, 0, len(objs))
	for _, co := range objs {
		obj, err := ObjectFromCustomObject(s.k, co)
		if err != nil {
			continue
		}
		out = append(out, listEntry{key: obj.Key, modifyIndex: obj.ModifyIndex, obj: obj})
	}
	return out, index, nil
}

func (s *nativeStore) write(ctx context.Context, obj *Object) (*Object, error) {
	co, err := obj.ToCustomObject()
	if err != nil {
		return nil, err
	}
	out, err := s.objs.ApplyObject(ctx, co, obj.ModifyIndex)
	if err != nil {
		return nil, err
	}
	return ObjectFromCustomObject(s.k, out)
}

func (s *nativeStore) writeStatus(ctx context.Context, obj *Object, status json.RawMessage) (*Object, error) {
	if obj.ModifyIndex == 0 {
		return nil, errors.New("a status update requires the ModifyIndex of the object")
	}
	doc, err := decodeDocument(status)
	if err != nil {
		return nil, fmt.Errorf("invalid status of %s %s: %w", s.k, obj.Key, err)
	}
	out, err := s.objs.UpdateObjectStatus(ctx, s.k, obj.Key.Namespace, obj.Key.Name, doc, obj.ModifyIndex)
	if err != nil {
		return nil, err
	}
	return ObjectFromCustomObject(s.k, out)
}

func (s *nativeStore) delete(ctx context.Context, key ObjectKey, modifyIndex uint64) error {
	return s.objs.DeleteObject(ctx, s.k, key.Namespace, key.Name, modifyIndex)
}

// topics subscribes to the CustomObject topic with the "group/Kind" key,
// which matches the events of every object of the kind.
func (s *nativeStore) topics() map[api.Topic][]string {
	return map[api.Topic][]string{api.TopicCustomObject: {s.k.String()}}
}

// decodeEvent decodes a CustomObject event. The events carry the whole
// object, so nothing has to be read.
func (s *nativeStore) decodeEvent(ev *api.Event) (objectChange, bool) {
	if ev.Topic != api.TopicCustomObject {
		return objectChange{}, false
	}
	switch ev.Type {
	case api.CustomObjectUpserted, api.CustomObjectStatusUpdated, api.CustomObjectDeleted:
	default:
		// Kind events share the topic and the key.
		return objectChange{}, false
	}
	co, err := ev.CustomObject()
	if err != nil || co == nil {
		return objectChange{}, false
	}
	obj, err := ObjectFromCustomObject(s.k, co)
	if err != nil {
		return objectChange{}, false
	}
	change := objectChange{key: obj.Key, modifyIndex: obj.ModifyIndex, obj: obj}
	if ev.Type == api.CustomObjectDeleted {
		change.obj, change.deleted = nil, true
	}
	return change, true
}
