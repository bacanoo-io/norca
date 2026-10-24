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
	kind Kind
	vars VariablesAPI
}

// NewObjects returns an object client for the kind.
func NewObjects(kind Kind, vars VariablesAPI) (*Objects, error) {
	if err := kind.Validate(); err != nil {
		return nil, err
	}
	return &Objects{kind: kind, vars: vars}, nil
}

// Kind returns the kind of the objects.
func (c *Objects) Kind() Kind { return c.kind }

// Get reads an object from the server. It returns an error wrapping
// ErrNotFound if the object does not exist.
func (c *Objects) Get(ctx context.Context, key ObjectKey) (*Object, error) {
	if err := ValidateName(key.Name); err != nil {
		return nil, err
	}
	v, err := c.vars.ReadVariable(ctx, key.Namespace, c.kind.Path(key.Name))
	if err != nil {
		return nil, err
	}
	return ObjectFromVariable(c.kind, v)
}

// List reads every object of the kind in the namespace ("*" for all
// namespaces the token can access). Variables that are not valid objects are
// skipped. It returns the index of the listing.
func (c *Objects) List(ctx context.Context, namespace string) ([]*Object, uint64, error) {
	metas, index, err := c.vars.ListVariables(ctx, namespace, c.kind.Prefix(), 0)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*Object, 0, len(metas))
	for _, meta := range metas {
		if _, ok := c.kind.NameFromPath(meta.Path); !ok {
			continue
		}
		v, err := c.vars.ReadVariable(ctx, meta.Namespace, meta.Path)
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return nil, 0, err
		}
		obj, err := ObjectFromVariable(c.kind, v)
		if err != nil {
			continue
		}
		out = append(out, obj)
	}
	return out, index, nil
}

// Create creates the object. It fails with ErrConflict if it exists.
func (c *Objects) Create(ctx context.Context, obj *Object) (*Object, error) {
	obj = obj.Copy()
	obj.Kind = c.kind
	obj.ModifyIndex = 0
	return c.write(ctx, obj)
}

// Update writes the object with check-and-set on its ModifyIndex. It fails
// with an error wrapping ErrConflict if the object was modified since it was
// read; re-read it and retry.
func (c *Objects) Update(ctx context.Context, obj *Object) (*Object, error) {
	if obj.ModifyIndex == 0 {
		return nil, errors.New("update requires the ModifyIndex of the object")
	}
	obj = obj.Copy()
	obj.Kind = c.kind
	return c.write(ctx, obj)
}

func (c *Objects) write(ctx context.Context, obj *Object) (*Object, error) {
	v, err := obj.ToVariable()
	if err != nil {
		return nil, err
	}
	out, err := c.vars.WriteVariable(ctx, v)
	if err != nil {
		return nil, err
	}
	return ObjectFromVariable(c.kind, out)
}

// Delete deletes the object with check-and-set on modifyIndex.
func (c *Objects) Delete(ctx context.Context, key ObjectKey, modifyIndex uint64) error {
	return c.vars.DeleteVariable(ctx, key.Namespace, c.kind.Path(key.Name), modifyIndex)
}

// maxStatusRetries bounds the check-and-set retries of UpdateStatus.
const maxStatusRetries = 5

// UpdateStatus writes the status of the object, keeping its spec. The status
// is written with check-and-set; on a conflict (typically a concurrent spec
// change by a user) the object is re-read and the status is applied to the
// new version. The write is skipped if the stored status is already equal.
//
// It returns the stored object. If the object no longer exists it returns
// an error wrapping ErrNotFound.
func (c *Objects) UpdateStatus(ctx context.Context, obj *Object, status any) (*Object, error) {
	cur := obj.Copy()
	if err := cur.SetStatus(status); err != nil {
		return nil, err
	}
	desired := cur.Status

	for attempt := 0; ; attempt++ {
		if jsonEqual(obj.Status, desired) && obj.ModifyIndex != 0 {
			return obj, nil
		}
		next := obj.Copy()
		next.Status = desired
		out, err := c.Update(ctx, next)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, ErrConflict) || attempt >= maxStatusRetries {
			return nil, fmt.Errorf("writing the status of %s %s: %w", c.kind, obj.Key, err)
		}
		slog.Default().Debug("status write conflict, retrying", "kind", c.kind.String(), "object", obj.Key.String())
		obj, err = c.Get(ctx, obj.Key)
		if err != nil {
			return nil, err
		}
	}
}
