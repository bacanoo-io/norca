// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Objects reads and writes the objects of one kind.
type Objects struct {
	store objectStore
}

// NewObjects returns an object client for the kind, with objects stored as
// Variables.
func NewObjects(kind Kind, vars VariablesAPI) (*Objects, error) {
	if err := kind.Validate(); err != nil {
		return nil, err
	}
	return &Objects{store: &variablesStore{k: kind, vars: vars}}, nil
}

// NewNativeObjects returns an object client for native objects of a
// registered kind.
func NewNativeObjects(kind Kind, objs ObjectsAPI) (*Objects, error) {
	if err := kind.Validate(); err != nil {
		return nil, err
	}
	return &Objects{store: &nativeStore{k: kind, objs: objs}}, nil
}

// newObjects returns an object client for the storage.
func newObjects(storage Storage, kind Kind, backend Backend) (*Objects, error) {
	switch storage {
	case StorageVariables:
		return NewObjects(kind, backend)
	case StorageNative:
		return NewNativeObjects(kind, backend)
	}
	return nil, fmt.Errorf("invalid object storage %q", storage)
}

// Kind returns the kind of the objects.
func (c *Objects) Kind() Kind { return c.store.kind() }

// Storage returns where the objects are stored.
func (c *Objects) Storage() Storage { return c.store.storage() }

// Get reads an object from the server. It returns an error wrapping
// ErrNotFound if the object does not exist.
func (c *Objects) Get(ctx context.Context, key ObjectKey) (*Object, error) {
	if err := c.store.validateName(key.Name); err != nil {
		return nil, err
	}
	return c.store.get(ctx, key)
}

// List reads every object of the kind in the namespace ("*" for all
// namespaces the token can access). Variables that are not valid objects are
// skipped. It returns the index of the listing.
func (c *Objects) List(ctx context.Context, namespace string) ([]*Object, uint64, error) {
	entries, index, err := c.store.list(ctx, namespace, 0)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*Object, 0, len(entries))
	for _, e := range entries {
		obj := e.obj
		if obj == nil {
			obj, err = c.store.get(ctx, e.key)
			if errors.Is(err, ErrNotFound) || errors.Is(err, errInvalidObject) {
				continue
			} else if err != nil {
				return nil, 0, err
			}
		}
		out = append(out, obj)
	}
	return out, index, nil
}

// Create creates the object. It fails with ErrConflict if it exists.
func (c *Objects) Create(ctx context.Context, obj *Object) (*Object, error) {
	obj = obj.Copy()
	obj.Kind = c.store.kind()
	obj.ModifyIndex = 0
	return c.store.write(ctx, obj)
}

// Update writes the object with check-and-set on its ModifyIndex. It fails
// with an error wrapping ErrConflict if the object was modified since it was
// read; re-read it and retry.
//
// With native objects only the spec and the metadata (labels, managed by)
// are written; the status is written with UpdateStatus. With Variables the
// whole object, including the status, is written.
func (c *Objects) Update(ctx context.Context, obj *Object) (*Object, error) {
	if obj.ModifyIndex == 0 {
		return nil, errors.New("update requires the ModifyIndex of the object")
	}
	obj = obj.Copy()
	obj.Kind = c.store.kind()
	return c.store.write(ctx, obj)
}

// Delete deletes the object with check-and-set on modifyIndex.
func (c *Objects) Delete(ctx context.Context, key ObjectKey, modifyIndex uint64) error {
	return c.store.delete(ctx, key, modifyIndex)
}

// maxStatusRetries bounds the check-and-set retries of UpdateStatus.
const maxStatusRetries = 5

// UpdateStatus writes the status of the object, keeping its spec. The status
// is written with check-and-set; on a conflict (typically a concurrent spec
// change by a user) the object is re-read and the status is applied to the
// new version. The write is skipped if the stored status is already equal.
// Native objects are written on the status endpoint, which requires the
// "write-status" capability instead of "write".
//
// It returns the stored object. If the object no longer exists it returns
// an error wrapping ErrNotFound.
func (c *Objects) UpdateStatus(ctx context.Context, obj *Object, status any) (*Object, error) {
	cur := obj.Copy()
	if err := cur.SetStatus(status); err != nil {
		return nil, err
	}
	desired := cur.Status
	kind := c.store.kind()

	for attempt := 0; ; attempt++ {
		if jsonEqual(obj.Status, desired) && obj.ModifyIndex != 0 {
			return obj, nil
		}
		out, err := c.store.writeStatus(ctx, obj, desired)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, ErrConflict) || attempt >= maxStatusRetries {
			return nil, fmt.Errorf("writing the status of %s %s: %w", kind, obj.Key, err)
		}
		slog.Default().Debug("status write conflict, retrying", "kind", kind.String(), "object", obj.Key.String())
		obj, err = c.Get(ctx, obj.Key)
		if err != nil {
			return nil, err
		}
	}
}
