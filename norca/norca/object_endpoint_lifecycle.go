// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-memdb"
	metrics "github.com/hashicorp/go-metrics"
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

// checkOperatorsFinalizerChange refuses adding or removing the finalizer of
// the built-in operators (structs.OperatorsFinalizer) on the finalizers
// endpoint, except to management tokens (and clusters without ACLs). The
// servers put it on and take it off themselves, through Raft: removed by
// another caller, a tenant or a volume request goes without the clean-up of
// the servers; added to an object no built-in operator works on, it holds
// back the deletion for good. The other norca.io/ finalizers belong to the
// operators that run as jobs with their own tokens and stay unrestricted.
func checkOperatorsFinalizerChange(aclObj *acl.ACL, existing *structs.CustomObject, add, remove []string) error {
	if aclObj == nil || aclObj.IsManagement() {
		return nil
	}
	has := slices.Contains(existing.Finalizers, structs.OperatorsFinalizer)
	if (has && slices.Contains(remove, structs.OperatorsFinalizer)) ||
		(!has && slices.Contains(add, structs.OperatorsFinalizer)) {
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"the finalizer %q is managed by the servers; only a management token may add or remove it",
			structs.OperatorsFinalizer)
	}
	return nil
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
// owner object is filled in for a writer who may read that owner: to anyone
// else it would tell whether the owner exists.
func resolveObjectOwners(snap *state.StateSnapshot, aclObj *acl.ACL, namespace string, owners []*structs.ObjectOwnerReference) ([]*structs.ObjectOwnerReference, error) {
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
		if ref.ID == "" && ownerKind != nil &&
			aclObj.AllowObjectOperation(objectOwnerACLNamespace(ref.Namespace), ref.Kind, acl.ObjectCapabilityRead) {
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

// objectOwnerACLNamespace returns the namespace whose object rules decide on
// an owner: its namespace, or that of the cluster-scoped objects for an owner
// without one.
func objectOwnerACLNamespace(namespace string) string {
	if namespace == "" {
		return acl.ObjectClusterNamespace
	}
	return namespace
}

// allowObjectOwner reports whether the ACL allows naming the object of a
// kind ("group/Kind") in a namespace ("": cluster-scoped) as an owner: with
// "write" or "manage-finalizers" on it. The owners of objects
// (checkObjectOwnerAccess) and of jobs (checkJobOwnerMark) are checked with
// it. It reads no state: the answer does not tell whether the owner exists.
func allowObjectOwner(aclObj *acl.ACL, kindRef, namespace string) bool {
	aclNamespace := objectOwnerACLNamespace(namespace)
	return aclObj.AllowObjectOperation(aclNamespace, kindRef, acl.ObjectCapabilityWrite) ||
		aclObj.AllowObjectOperation(aclNamespace, kindRef, acl.ObjectCapabilityManageFinalizers)
}

// errObjectOwnerAccess is the error of naming an owner without the right on
// it (allowObjectOwner). It is the same whether the owner exists or not.
func errObjectOwnerAccess(kindRef, namespace, name string) error {
	where := "cluster-scoped"
	if namespace != "" {
		where = fmt.Sprintf("in namespace %q", namespace)
	}
	return structs.NewErrRPCCodedf(http.StatusForbidden,
		"owner %s %q %s: naming an owner requires the write or manage-finalizers capability on the owner",
		kindRef, name, where)
}

// checkObjectOwnerAccess refuses a new reference to an owner object unless
// the writer has "write" or "manage-finalizers" on the owner, wherever the
// owner is: in another namespace, cluster-scoped, or in the namespace of the
// object, of another kind or of its own. A dependent holds back the
// foreground deletion of its owner like a finalizer does, and the deletion
// of an owner is refused to a caller without the rights on its dependents,
// so the right to write the objects of one kind must not be enough to make
// them dependents of any other object. The owner need not exist. owners are
// resolved (resolveObjectOwners); the references the stored object already
// has are kept without the check.
func checkObjectOwnerAccess(aclObj *acl.ACL, existing *structs.CustomObject, owners []*structs.ObjectOwnerReference) error {
	for _, ref := range owners {
		if !ref.IsObject() {
			continue
		}
		if existing != nil && slices.ContainsFunc(existing.Owners, func(o *structs.ObjectOwnerReference) bool {
			return o != nil && o.Kind == ref.Kind && o.Namespace == ref.Namespace && o.Name == ref.Name
		}) {
			continue
		}
		if !allowObjectOwner(aclObj, ref.Kind, ref.Namespace) {
			return errObjectOwnerAccess(ref.Kind, ref.Namespace, ref.Name)
		}
	}
	return nil
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
	return s.removeCustomObjectApproved(obj, propagation, nil)
}

// removeCustomObjectApproved removes an object as removeCustomObject does,
// and records in its tombstone the dependents the garbage collector may
// delete although their deletion destroys data in the storage (see
// CustomObject.DeletionApproved). For an object that is marked for deletion
// the list of the mark is recorded instead.
func (s *Server) removeCustomObjectApproved(obj *structs.CustomObject, propagation string, approved []string) (uint64, error) {
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
		ApprovedDependents:  approved,
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
		if isCertificateKind(kind.Group, kind.Kind) {
			if err := checkCertificateDelete(aclObj, existing); err != nil {
				return err
			}
		}
		// What the deletion makes the servers delete in the storage.
		if isSnapshotKind(kind.Group, kind.Kind) {
			if err := checkSnapshotDelete(aclObj, existing); err != nil {
				return err
			}
		}
		if isVolumeRequestKind(kind.Group, kind.Kind) {
			if err := checkVolumeRequestDelete(aclObj, existing); err != nil {
				return err
			}
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

		// What the deletion takes with it: the garbage collector deletes
		// the dependent objects and stops the dependent jobs with the
		// rights of the servers, so the caller needs the rights for the
		// objects whose deletion deletes storage and for the jobs. The
		// ones the caller was checked for are recorded with the deletion:
		// the collector takes no other one of those.
		var approved []string
		if propagation != structs.ObjectPropagationOrphan {
			if approved, err = checkObjectDependentsDelete(snap, aclObj, existing); err != nil {
				return err
			}
		}

		// Validating type-bound webhooks that review deletions. They
		// review again after a conflict: the object they are asked about
		// is the changed one.
		if len(kind.Webhooks) > 0 {
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
			marked.DeletionApproved = approved
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
			index, err = c.srv.removeCustomObjectApproved(existing, propagation, approved)
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
		build: func(kind *structs.ObjectKind, _ string, existing *structs.CustomObject, aclObj *acl.ACL, _ *state.StateSnapshot) (*structs.CustomObject, string, error) {
			if existing == nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kind.KindRef(), args.Name)
			}
			if err := checkOperatorsFinalizerChange(aclObj, existing, args.Add, args.Remove); err != nil {
				return nil, "", err
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

	if isBuiltinObjectGroup(args.Group) {
		return errBuiltinObjectGroup(args.Group)
	}

	if err := structs.ValidateObjectKindName(args.Group, args.Kind); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	switch {
	case len(args.Events) == 0:
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "missing events")
	case len(args.Events) > structs.MaxObjectEventsPerRequest:
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "at most %d events can be written at once", structs.MaxObjectEventsPerRequest)
	}
	// The autoscaler takes its cooldown, the directions of its changes and
	// its suspension from the log: the "Scaled" reason is written by the
	// scale subresource only, and the autoscaler's meta by its identity.
	byAutoscaler := c.srv.isAutoscalerRequest(args)
	events := make([]*structs.ObjectEvent, len(args.Events))
	for i, e := range args.Events {
		if e == nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "event %d: empty", i)
		}
		if err := e.Validate(); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "event %d: %v", i, err)
		}
		if e.Reason == structs.ObjectScaleReason {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"event %d: the reason %q is reserved for the scale subresource", i, e.Reason)
		}
		if err := checkAutoscalerMeta(byAutoscaler, e.Meta); err != nil {
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
	if err := checkAutoscalerMeta(c.srv.isAutoscalerRequest(args), args.Meta); err != nil {
		return err
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
