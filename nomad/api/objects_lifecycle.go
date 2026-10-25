// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"

	"github.com/mitchellh/mapstructure"
)

// Lifecycle of custom objects: finalizers, deletion modes, owner
// references, the object event log and the scale subresource.

const (
	// Deletion propagation modes. Background (the default) deletes the
	// object at once and what it owns afterwards; foreground deletes what
	// it owns first; orphan leaves what it owns in place.
	ObjectPropagationBackground = "background"
	ObjectPropagationForeground = "foreground"
	ObjectPropagationOrphan     = "orphan"

	// CustomObjectEventRecorded is the event stream type of new entries of
	// an object's event log (topic CustomObject).
	CustomObjectEventRecorded = "CustomObjectEventRecorded"

	// Types of object log events.
	ObjectEventTypeNormal  = "Normal"
	ObjectEventTypeWarning = "Warning"

	// JobMetaOwner is the reserved job meta key referencing the custom
	// object that owns a job: "<namespace>/crd/<group with dots replaced
	// by dashes>/<Kind>/<name>". JobMetaOwnerKind optionally holds the
	// owner's "group/Kind". The object garbage collector stops the jobs of
	// deleted owners (except in the orphan mode).
	JobMetaOwner     = "norca.owner"
	JobMetaOwnerKind = "norca.owner.kind"

	// JobMetaManagedBy is the reserved job meta key naming the identity
	// that manages a job ("workload:<namespace>/<job>", "role:<name>",
	// "policy:<name>" or "token:<accessor ID>"). Other identities can't
	// update, stop, revert or scale it (groups with their own scaling block
	// can still be scaled).
	JobMetaManagedBy = "norca.managed-by"

	// ObjectScaleReason is the reason of the log event recorded when the
	// scale subresource changes the replica count.
	ObjectScaleReason = "Scaled"
)

// Deleting reports whether the object is marked for deletion.
func (o *CustomObject) Deleting() bool {
	return o.DeletionTime != 0
}

// ObjectEvent is an entry of the event log of an object.
type ObjectEvent struct {
	// Type is ObjectEventTypeNormal (the default) or ObjectEventTypeWarning.
	Type string

	// Reason is a short CamelCase reason, Message the human-readable text.
	Reason  string
	Message string

	// Reporter is the identity that wrote the event (set by the server).
	Reporter string

	// Meta holds machine-readable details.
	Meta map[string]string

	// Count is how often the event repeated between FirstTime and LastTime
	// (UnixNano). Set by the server.
	Count     int
	FirstTime int64
	LastTime  int64
	Index     uint64
}

// ObjectScale is the scale subresource of an object.
type ObjectScale struct {
	Group     string
	Kind      string
	Namespace string
	Name      string

	// Replicas is the desired count at the kind's spec path;
	// StatusReplicas the observed count at its status path, if reported.
	Replicas       int64
	StatusReplicas *int64

	SpecPath   string
	StatusPath string

	Generation  uint64
	ModifyIndex uint64
}

// CustomObjectEventsEvent is the payload of CustomObjectEventRecorded
// events: the new entries of the log of an object.
type CustomObjectEventsEvent struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Events    []*ObjectEvent
}

// CustomObjectEvents returns the payload of a CustomObjectEventRecorded
// event.
func (e *Event) CustomObjectEvents() (*CustomObjectEventsEvent, error) {
	var out CustomObjectEventsEvent
	if err := mapstructure.Decode(e.Payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateFinalizers adds and removes finalizers of an object. It requires
// the "manage-finalizers" capability. Removing the last finalizer of an
// object marked for deletion deletes it. It returns the object as stored.
func (o *Objects) UpdateFinalizers(group, kind, name string, add, remove []string, opts *ObjectWriteOptions, w *WriteOptions) (*CustomObject, *WriteMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, nil, err
	}
	body := map[string]any{"Add": add, "Remove": remove}
	var resp CustomObject
	wm, err := o.client.put(endpoint+"/finalizers"+opts.query(), body, &resp, w)
	if err != nil {
		return nil, nil, objectWriteError(err)
	}
	return &resp, wm, nil
}

// Events returns the event log of an object, oldest event first.
func (o *Objects) Events(group, kind, name string, q *QueryOptions) ([]*ObjectEvent, *QueryMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, nil, err
	}
	var resp []*ObjectEvent
	qm, err := o.client.query(endpoint+"/events", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// RecordEvents adds events to the event log of an object. It requires the
// "write-events" capability. Repetitions of the newest event are counted.
func (o *Objects) RecordEvents(group, kind, name string, events []*ObjectEvent, w *WriteOptions) (*WriteMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, errors.New("missing events")
	}
	return o.client.put(endpoint+"/events", map[string]any{"Events": events}, nil, w)
}

// Scale returns the scale subresource of an object.
func (o *Objects) Scale(group, kind, name string, q *QueryOptions) (*ObjectScale, *QueryMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, nil, err
	}
	var resp ObjectScale
	qm, err := o.client.query(endpoint+"/scale", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// SetScale sets the replica count of an object on its scale subresource.
// The message is recorded in the object's event log.
func (o *Objects) SetScale(group, kind, name string, replicas int64, message string, opts *ObjectWriteOptions, w *WriteOptions) (*ObjectScale, *WriteMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, nil, err
	}
	body := map[string]any{"Replicas": replicas, "Message": message}
	var resp ObjectScale
	wm, err := o.client.put(endpoint+"/scale"+opts.query(), body, &resp, w)
	if err != nil {
		return nil, nil, objectWriteError(err)
	}
	return &resp, wm, nil
}
