// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/hashicorp/nomad/api"
)

// Storage selects where the objects of a kind are stored.
type Storage string

const (
	// StorageVariables stores objects as Variables under
	// crd/<group>/<kind>/<name> (the phase 1 prototype). It is the default.
	StorageVariables Storage = ""

	// StorageNative uses native custom objects of a kind registered with
	// "norca object-kind apply": the server validates them against the
	// kind's schema and CEL rules, fills in default values, maintains the
	// generation and serves the status on a separate endpoint.
	StorageNative Storage = "native"
)

// ParseStorage parses "variables" (or "") and "native".
func ParseStorage(s string) (Storage, error) {
	switch s {
	case "", "variables":
		return StorageVariables, nil
	case "native":
		return StorageNative, nil
	}
	return "", fmt.Errorf("invalid object storage %q: use variables or native", s)
}

// ObjectFromCustomObject converts a native object of the kind into an
// Object.
func ObjectFromCustomObject(kind Kind, co *api.CustomObject) (*Object, error) {
	if co == nil {
		return nil, errors.New("nil object")
	}
	if co.Group != kind.Group || co.Kind != kind.Kind {
		return nil, fmt.Errorf("object %s/%s is not of kind %s", co.KindRef(), co.Name, kind)
	}
	o := &Object{
		Kind:        kind,
		Key:         ObjectKey{Namespace: co.Namespace, Name: co.Name},
		ID:          co.ID,
		Generation:  co.Generation,
		ManagedBy:   co.ManagedBy,
		CreateIndex: co.CreateIndex,
		ModifyIndex: co.ModifyIndex,
		CreateTime:  time.Unix(0, co.CreateTime),
		ModifyTime:  time.Unix(0, co.ModifyTime),
		Labels:      maps.Clone(co.Labels),
		Finalizers:  slices.Clone(co.Finalizers),
		Owners:      copyOwners(co.Owners),
	}
	if co.DeletionTime != 0 {
		o.DeletionTime = time.Unix(0, co.DeletionTime)
	}
	if len(co.Spec) > 0 {
		b, err := json.Marshal(co.Spec)
		if err != nil {
			return nil, fmt.Errorf("the spec of %s %s can't be encoded: %w", kind, o.Key, err)
		}
		o.Spec = b
	}
	if co.Status != nil {
		b, err := json.Marshal(co.Status)
		if err != nil {
			return nil, fmt.Errorf("the status of %s %s can't be encoded: %w", kind, o.Key, err)
		}
		o.Status = b
	}
	return o, nil
}

// ToCustomObject converts the object into a native object. The status is
// included, although spec writes ignore it.
func (o *Object) ToCustomObject() (*api.CustomObject, error) {
	if err := ValidateNativeName(o.Key.Name); err != nil {
		return nil, err
	}
	co := &api.CustomObject{
		ID:          o.ID,
		Group:       o.Kind.Group,
		Kind:        o.Kind.Kind,
		Namespace:   o.Key.Namespace,
		Name:        o.Key.Name,
		Labels:      maps.Clone(o.Labels),
		ManagedBy:   o.ManagedBy,
		Generation:  o.Generation,
		CreateIndex: o.CreateIndex,
		ModifyIndex: o.ModifyIndex,
		// The server adds the finalizers of a write and never removes any.
		Finalizers: slices.Clone(o.Finalizers),
		Owners:     copyOwners(o.Owners),
	}
	var err error
	if co.Spec, err = decodeDocument(o.Spec); err != nil {
		return nil, fmt.Errorf("invalid spec of %s %s: %w", o.Kind, o.Key, err)
	}
	if co.Status, err = decodeDocument(o.Status); err != nil {
		return nil, fmt.Errorf("invalid status of %s %s: %w", o.Kind, o.Key, err)
	}
	return co, nil
}

// decodeDocument decodes a JSON object; an empty document is nil.
func decodeDocument(doc json.RawMessage) (map[string]any, error) {
	if len(doc) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func copyOwners(owners []*api.ObjectOwnerReference) []*api.ObjectOwnerReference {
	if owners == nil {
		return nil
	}
	out := make([]*api.ObjectOwnerReference, 0, len(owners))
	for _, ref := range owners {
		if ref != nil {
			r := *ref
			out = append(out, &r)
		}
	}
	return out
}