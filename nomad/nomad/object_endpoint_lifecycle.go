// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Lifecycle of custom objects: deletion with finalizers and propagation
// modes, the finalizers and events endpoints, and the scale subresource.

const (
	objectOperationScale = "scale"
)

// errObjectDeleting is the error of adding finalizers to an object marked
// for deletion.
func errObjectDeleting(obj *structs.CustomObject) error {
	return structs.NewErrRPCCodedf(http.StatusConflict,
		"%s %q is being deleted; finalizers can't be added", obj.KindRef(), obj.Name)
}

// checkObjectManagedBy enforces the managed_by identity of an object.
// Management tokens (and clusters without ACLs) are not restricted.
func checkObjectManagedBy(aclObj *acl.ACL, args structs.RequestWithIdentity, obj *structs.CustomObject) error {
	if obj.ManagedBy == "" || aclObj == nil || aclObj.IsManagement() {
		return nil
	}
	enforced, allowed := structs.ManagedByAllows(obj.ManagedBy, admissionUser(args))
	if enforced && !allowed {
		return structs.NewObjectManagedError(fmt.Sprintf("%s %q", obj.KindRef(), obj.Name), obj.ManagedBy)
	}
	return nil
}

// resolveObjectOwners fills in the defaults of owner references: the
// namespace of a namespaced owner defaults to the object's namespace, the
// namespace of a cluster-scoped owner is empty, and the ID of an existing
// owner object is filled in.
func resolveObjectOwners(snap *state.StateSnapshot, namespace string, owners []*structs.ObjectOwnerReference) ([]*structs.ObjectOwnerReference, error) {
	if owners == nil {
		return nil, nil
	}
	out := make([]*structs.ObjectOwnerReference, 0, len(owners))
	for _, in := range owners {
		ref := *in
		if !ref.IsObject() {
			if ref.Namespace == "" {
				ref.Namespace = namespace
			}
			out = append(out, &ref)
			continue
		}
		group, kind, err := structs.ParseObjectKindRef(ref.Kind)
		if err != nil {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid owner: %v", err)
		}
		ownerKind, err := snap.ObjectKindByName(nil, group, kind)
		if err != nil {
			return nil, err
		}
		switch {
		case ownerKind != nil && !ownerKind.Namespaced:
			ref.Namespace = ""
		case ref.Namespace == "":
			ref.Namespace = namespace
		}
		if ref.ID == "" && ownerKind != nil {
			owner, err := snap.CustomObjectByName(nil, group, kind, ref.Namespace, ref.Name)
			if err != nil {
				return nil, err
			}
			if owner != nil {
				ref.ID = owner.ID
			}
		}
		out = append(out, &ref)
	}
	return out, nil
}

// objectReporter returns the name of the identity recorded as the reporter
// of object events.
func objectReporter(args structs.RequestWithIdentity) string {
	user := admissionUser(args)
	switch {
	case user.Name != "":
		return user.Name
	case user.AccessorID != "":
		return structs.ManagedByTokenPrefix + user.AccessorID
	}
	return user.Type
}

// removeCustomObject removes an object from the state store with
// check-and-set on its modify index, leaving a tombstone for the background
// and foreground propagation modes.
func (s *Server) removeCustomObject(obj *structs.CustomObject, propagation string) (uint64, error) {
	if propagation == "" {
		propagation = structs.ObjectPropagationBackground
	}
	req := &structs.CustomObjectDeleteStateRequest{
		Group:               obj.Group,
		Kind:                obj.Kind,
		Namespace:           obj.Namespace,
		Name:                obj.Name,
		ExpectedModifyIndex: obj.ModifyIndex,
		Propagation:         propagation,
		DeleteTime:          time.Now().UnixNano(),
		WriteRequest:        structs.WriteRequest{Region: s.Region()},
	}
	_, index, err := s.raftApply(structs.CustomObjectDeleteRequestType|structs.IgnoreUnknownTypeFlag, req)
	return index, err
}

// finalizeObjectIfReady removes an object marked for deletion once its
// finalizers are gone, unless the foreground mode waits for the garbage
// collector to delete its dependents first. A concurrent change is left to
// the garbage collector.
func (s *Server) finalizeObjectIfReady(obj *structs.CustomObject) (uint64, bool) {
	if !obj.Deleting() || len(obj.Finalizers) > 0 || obj.DeletionPropagation == structs.ObjectPropagationForeground {
		return 0, false
	}
	index, err := s.removeCustomObject(obj, obj.DeletionPropagation)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			s.logger.Warn("failed to remove finalized object", "object", obj.KindRef()+"/"+obj.Name,
				"namespace", obj.Namespace, "error", err)
		}
		return 0, false
	}
	return index, true
}

// recordObjectEvents appends events to the log of an object.
func (s *Server) recordObjectEvents(obj *structs.CustomObject, events []*structs.ObjectEvent, reporter string) (uint64, error) {
	for _, e := range events {
		e.Reporter = reporter
	}
	req := &structs.CustomObjectEventsStateRequest{
		Group:        obj.Group,
		Kind:         obj.Kind,
		Namespace:    obj.Namespace,
		Name:         obj.Name,
		ObjectID:     obj.ID,
		Events:       events,
		Time:         time.Now().UnixNano(),
		Limit:        structs.DefaultObjectEventLogLimit,
		WriteRequest: structs.WriteRequest{Region: s.Region()},
	}
	_, index, err := s.raftApply(structs.CustomObjectEventsRequestType|structs.IgnoreUnknownTypeFlag, req)
	return index, err
}

// Delete deletes an object. An object without finalizers is removed at
// once, unless the foreground mode is requested; otherwise it is marked for
// deletion (DeletionTime) and removed when its finalizers are gone and, in
// the foreground mode, the garbage collector has deleted its dependents.
// The reply is the object as marked, or as it was before it was removed
// (DeletionTime 0).
func (c *CustomObject) Delete(args *structs.CustomObjectDeleteRequest, reply *structs.CustomObjectWriteResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.Delete", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "delete"}, time.Now())

	if isBuiltinObjectGroup(args.Group) {
		return errBuiltinObjectGroup(args.Group)
	}

	if err := structs.ValidateObjectPropagation(args.Propagation); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	propagation := args.Propagation
	if propagation == "" {
		propagation = structs.ObjectPropagationBackground
	}

	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	kindRef := structs.ObjectKindRef(args.Group, args.Kind)

	for attempt := 1; ; attempt++ {
		snap, err := c.srv.State().Snapshot()
		if err != nil {
			return err
		}
		kind, err := snap.ObjectKindByName(nil, args.Group, args.Kind)
		if err != nil {
			return err
		}
		if kind == nil {
			if !aclObj.AllowObjectOperation(args.RequestNamespace(), kindRef, acl.ObjectCapabilityWrite) {
				return structs.ErrPermissionDenied
			}
			return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", kindRef)
		}
		namespace := objectNamespace(kind, args.RequestNamespace())
		if !aclObj.AllowObjectOperation(objectACLNamespace(kind, namespace), kindRef, acl.ObjectCapabilityWrite) {
			return structs.ErrPermissionDenied
		}

		existing, err := snap.CustomObjectByName(nil, args.Group, args.Kind, namespace, args.Name)
		if err != nil {
			return err
		}
		if existing == nil {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kindRef, args.Name)
		}
		if args.CAS != nil && existing.ModifyIndex != *args.CAS {
			return structs.NewObjectConflictError("%s %q has modify index %d, expected %d",
				kindRef, args.Name, existing.ModifyIndex, *args.CAS)
		}
		if err := checkObjectManagedBy(aclObj, args, existing); err != nil {
			return err
		}

		if attempt == 1 {
			if err := c.srv.checkObjectWriteLimits(kind, args); err != nil {
				return err
			}
		}

		// A repeated deletion of an object being deleted changes nothing.
		if existing.Deleting() {
			reply.Object = existing
			reply.Unchanged = true
			reply.Index = existing.ModifyIndex
			return nil
		}

		// Validating type-bound webhooks that review deletions.
		if attempt == 1 && len(kind.Webhooks) > 0 {
			review := &objectReview{kind: kind, operation: objectOperationDelete, obj: existing,
				existing: existing, args: args, dryRun: args.DryRun}
			warnings, err := c.srv.runValidatingWebhooks(review)
			reply.Warnings = warnings
			if err != nil {
				return err
			}
		}

		removeNow := len(existing.Finalizers) == 0 && propagation != structs.ObjectPropagationForeground
		var marked *structs.CustomObject
		if !removeNow {
			marked = existing.Copy()
			marked.DeletionTime = time.Now().UnixNano()
			marked.DeletionPropagation = propagation
			marked.ModifyTime = marked.DeletionTime
		}
		if args.DryRun {
			reply.Object = existing
			if marked != nil {
				reply.Object = marked
			}
			return nil
		}

		if !ServersMeetMinimumNorcaVersion(c.srv.serf.Members(), c.srv.Region(), false) {
			return fmt.Errorf("all servers must be running Norca %v or later to delete custom objects", minNorcaFeaturesVersion)
		}

		var index uint64
		if removeNow {
			index, err = c.srv.removeCustomObject(existing, propagation)
		} else {
			req := &structs.CustomObjectUpsertStateRequest{
				Object:              marked,
				ExpectedModifyIndex: existing.ModifyIndex,
				WriteRequest:        structs.WriteRequest{Region: c.srv.Region()},
			}
			_, index, err = c.srv.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
		}
		if err != nil {
			if structs.IsObjectConflictError(err) && args.CAS == nil && attempt < objectWriteAttempts {
				time.Sleep(rand.N(time.Duration(attempt) * objectWriteBackoff))
				continue
			}
			return err
		}

		reply.Object = existing
		if marked != nil {
			marked.ModifyIndex = index
			reply.Object = marked
		}
		reply.Index = index
		return nil
	}
}

// UpdateFinalizers adds and removes finalizers of an object. Removing the
// last finalizer of an object marked for deletion removes the object
// (unless the foreground mode still waits for its dependents).
func (c *CustomObject) UpdateFinalizers(args *structs.CustomObjectFinalizersRequest, reply *structs.CustomObjectWriteResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.UpdateFinalizers", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "update_finalizers"}, time.Now())

	if isBuiltinObjectGroup(args.Group) {
		return errBuiltinObjectGroup(args.Group)
	}

	if err := structs.ValidateObjectKindName(args.Group, args.Kind); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	if err := structs.ValidateObjectFinalizers(args.Add); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	w := &objectWrite{
		args:         args,
		group:        args.Group,
		kind:         args.Kind,
		namespace:    args.RequestNamespace(),
		name:         args.Name,
		cas:          args.CAS,
		dryRun:       args.DryRun,
		capability:   acl.ObjectCapabilityManageFinalizers,
		metadataOnly: true,
		build: func(kind *structs.ObjectKind, _ string, existing *structs.CustomObject, _ *acl.ACL, _ *state.StateSnapshot) (*structs.CustomObject, string, error) {
			if existing == nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kind.KindRef(), args.Name)
			}
			obj := existing.Copy()
			obj.Finalizers = slices.DeleteFunc(obj.Finalizers, func(f string) bool {
				return slices.Contains(args.Remove, f)
			})
			for _, f := range args.Add {
				if slices.Contains(obj.Finalizers, f) {
					continue
				}
				if obj.Deleting() {
					return nil, "", errObjectDeleting(obj)
				}
				obj.Finalizers = append(obj.Finalizers, f)
			}
			if len(obj.Finalizers) == 0 {
				obj.Finalizers = nil
			}
			if err := structs.ValidateObjectFinalizers(obj.Finalizers); err != nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
			}
			obj.ModifyTime = time.Now().UnixNano()
			return obj, objectOperationUpdate, nil
		},
	}
	if err := c.writeObject(w, reply); err != nil {
		return err
	}
	if !args.DryRun && reply.Object != nil {
		if index, ok := c.srv.finalizeObjectIfReady(reply.Object); ok {
			reply.Index = index
		}
	}
	return nil
}

// RecordEvents adds events to the event log of an object.
func (c *CustomObject) RecordEvents(args *structs.CustomObjectEventsRequest, reply *structs.GenericResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.RecordEvents", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "record_events"}, time.Now())

	if err := structs.ValidateObjectKindName(args.Group, args.Kind); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	switch {
	case len(args.Events) == 0:
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "missing events")
	case len(args.Events) > structs.MaxObjectEventsPerRequest:
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "at most %d events can be written at once", structs.MaxObjectEventsPerRequest)
	}
	events := make([]*structs.ObjectEvent, len(args.Events))
	for i, e := range args.Events {
		if e == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "event %d: empty", i)
		}
		if err := e.Validate(); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "event %d: %v", i, err)
		}
		events[i] = &structs.ObjectEvent{Type: e.Type, Reason: e.Reason, Message: e.Message, Meta: maps.Clone(e.Meta)}
	}

	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	obj, err := c.authorizedObject(aclObj, args.Group, args.Kind, args.RequestNamespace(), args.Name, acl.ObjectCapabilityWriteEvents)
	if err != nil {
		return err
	}
	if kind, err := c.srv.State().ObjectKindByName(nil, args.Group, args.Kind); err != nil {
		return err
	} else if kind != nil {
		if err := c.srv.checkObjectWriteLimits(kind, args); err != nil {
			return err
		}
	}

	if !ServersMeetMinimumNorcaVersion(c.srv.serf.Members(), c.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to record object events", minNorcaFeaturesVersion)
	}
	index, err := c.srv.recordObjectEvents(obj, events, objectReporter(args))
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// authorizedObject reads an object for a write that requires the
// capability. It fails with 404 if the kind or the object does not exist.
func (c *CustomObject) authorizedObject(aclObj *acl.ACL, group, kindName, requested, name, capability string) (*structs.CustomObject, error) {
	kindRef := structs.ObjectKindRef(group, kindName)
	snap, err := c.srv.State().Snapshot()
	if err != nil {
		return nil, err
	}
	kind, err := snap.ObjectKindByName(nil, group, kindName)
	if err != nil {
		return nil, err
	}
	if kind == nil {
		if !aclObj.AllowObjectOperation(requested, kindRef, capability) {
			return nil, structs.ErrPermissionDenied
		}
		return nil, structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", kindRef)
	}
	namespace := objectNamespace(kind, requested)
	if !aclObj.AllowObjectOperation(objectACLNamespace(kind, namespace), kindRef, capability) {
		return nil, structs.ErrPermissionDenied
	}
	obj, err := snap.CustomObjectByName(nil, group, kindName, namespace, name)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kindRef, name)
	}
	return obj, nil
}

// ListEvents returns the event log of an object, oldest event first. It
// supports blocking queries.
func (c *CustomObject) ListEvents(args *structs.CustomObjectSpecificRequest, reply *structs.CustomObjectEventsResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.ListEvents", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "list_events"}, time.Now())

	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	kindRef := structs.ObjectKindRef(args.Group, args.Kind)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			reply.Events = nil
			kind, err := store.ObjectKindByName(ws, args.Group, args.Kind)
			if err != nil {
				return err
			}
			namespace := args.RequestNamespace()
			aclNamespace := namespace
			if kind != nil {
				namespace = objectNamespace(kind, namespace)
				aclNamespace = objectACLNamespace(kind, namespace)
			}
			if !aclObj.AllowObjectOperation(aclNamespace, kindRef, acl.ObjectCapabilityRead) {
				return structs.ErrPermissionDenied
			}
			if kind == nil {
				return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", kindRef)
			}

			log, err := store.ObjectEventLog(ws, args.Group, args.Kind, namespace, args.Name)
			if err != nil {
				return err
			}
			reply.Events = []*structs.ObjectEvent{}
			if log != nil {
				reply.Events = log.Events
				reply.Index = log.ModifyIndex
			} else {
				index, err := store.Index(state.TableObjectEvents)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			c.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return c.srv.blockingRPC(&opts)
}

// GetScale returns the scale subresource of an object.
func (c *CustomObject) GetScale(args *structs.CustomObjectSpecificRequest, reply *structs.SingleObjectScaleResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.GetScale", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "get_scale"}, time.Now())

	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	kindRef := structs.ObjectKindRef(args.Group, args.Kind)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			reply.Scale = nil
			kind, err := store.ObjectKindByName(ws, args.Group, args.Kind)
			if err != nil {
				return err
			}
			namespace := args.RequestNamespace()
			aclNamespace := namespace
			if kind != nil {
				namespace = objectNamespace(kind, namespace)
				aclNamespace = objectACLNamespace(kind, namespace)
			}
			if !aclObj.AllowObjectOperation(aclNamespace, kindRef, acl.ObjectCapabilityRead) {
				return structs.ErrPermissionDenied
			}
			switch {
			case kind == nil:
				return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", kindRef)
			case kind.Scale == nil:
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "object kind %q has no scale subresource", kindRef)
			}
			obj, err := store.CustomObjectByName(ws, args.Group, args.Kind, namespace, args.Name)
			if err != nil {
				return err
			}
			if obj == nil {
				return structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kindRef, args.Name)
			}
			reply.Scale = structs.ScaleOf(kind, obj)
			reply.Index = obj.ModifyIndex
			c.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return c.srv.blockingRPC(&opts)
}

// Scale sets the replica count of an object on its scale subresource. The
// spec is validated as for other writes; the managed_by identity is not
// enforced. A change is recorded in the event log with the reason "Scaled",
// which the autoscaler derives its cooldown from.
func (c *CustomObject) Scale(args *structs.CustomObjectScaleRequest, reply *structs.CustomObjectScaleResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.Scale", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "scale"}, time.Now())

	if isBuiltinObjectGroup(args.Group) {
		return errBuiltinObjectGroup(args.Group)
	}

	if err := structs.ValidateObjectKindName(args.Group, args.Kind); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	if args.Replicas < 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "replicas must not be negative")
	}
	event := &structs.ObjectEvent{Reason: structs.ObjectScaleReason, Message: args.Message, Meta: maps.Clone(args.Meta)}
	if err := event.Validate(); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	var previous int64
	var scaleKind *structs.ObjectKind
	w := &objectWrite{
		args:       args,
		group:      args.Group,
		kind:       args.Kind,
		namespace:  args.RequestNamespace(),
		name:       args.Name,
		cas:        args.CAS,
		dryRun:     args.DryRun,
		capability: acl.ObjectCapabilityWrite,
		build: func(kind *structs.ObjectKind, _ string, existing *structs.CustomObject, _ *acl.ACL, _ *state.StateSnapshot) (*structs.CustomObject, string, error) {
			if kind.Scale == nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusBadRequest, "object kind %q has no scale subresource", kind.KindRef())
			}
			if existing == nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kind.KindRef(), args.Name)
			}
			scaleKind = kind
			previous = structs.ScaleOf(kind, existing).Replicas
			obj := existing.Copy()
			if err := structs.SetObjectPathValue(obj, kind.Scale.SpecPath, float64(args.Replicas)); err != nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusBadRequest, "scale: %v", err)
			}
			if !structs.ObjectMapsEqual(existing.Spec, obj.Spec) {
				obj.Generation++
				obj.ModifyTime = time.Now().UnixNano()
			}
			return obj, objectOperationScale, nil
		},
	}
	var out structs.CustomObjectWriteResponse
	if err := c.writeObject(w, &out); err != nil {
		return err
	}
	reply.Scale = structs.ScaleOf(scaleKind, out.Object)
	reply.Unchanged = out.Unchanged
	reply.Index = out.Index
	if args.DryRun || out.Unchanged {
		return nil
	}

	if event.Message == "" {
		event.Message = fmt.Sprintf("scaled from %d to %d", previous, args.Replicas)
	}
	if event.Meta == nil {
		event.Meta = map[string]string{}
	}
	event.Meta[structs.ObjectEventMetaCount] = strconv.FormatInt(args.Replicas, 10)
	event.Meta[structs.ObjectEventMetaPreviousCount] = strconv.FormatInt(previous, 10)
	if _, err := c.srv.recordObjectEvents(out.Object, []*structs.ObjectEvent{event}, objectReporter(args)); err != nil {
		c.srv.logger.Warn("failed to record the scaling event", "object", out.Object.KindRef()+"/"+out.Object.Name,
			"namespace", out.Object.Namespace, "error", err)
	}
	return nil
}
