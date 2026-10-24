// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/objectschema"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/state/paginator"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// objectWriteAttempts is how often a write without an explicit
	// check-and-set index is retried when a concurrent write changed the
	// object between reading and applying it.
	objectWriteAttempts = 10

	// objectWriteBackoff is the base of the randomized backoff between
	// write attempts.
	objectWriteBackoff = 5 * time.Millisecond

	// Operations of custom object writes, as CEL rules see them in
	// request.operation.
	objectOperationCreate = "create"
	objectOperationUpdate = "update"
	objectOperationStatus = "status"
)

// objectRegistry is the type registry of custom objects: it caches the
// compiled schemas and CEL rules of the object kinds.
type objectRegistry struct {
	rules *admission.ObjectRuleEngine

	lock    sync.Mutex
	schemas map[string]*compiledObjectSchema
}

type compiledObjectSchema struct {
	modifyIndex uint64
	schema      *objectschema.Schema
	err         error
}

func newObjectRegistry() (*objectRegistry, error) {
	rules, err := admission.NewObjectRuleEngine()
	if err != nil {
		return nil, err
	}
	return &objectRegistry{
		rules:   rules,
		schemas: make(map[string]*compiledObjectSchema),
	}, nil
}

// compile compiles the schema and the rules of a kind without caching
// them, and returns an error describing everything that does not compile.
func (r *objectRegistry) compile(kind *structs.ObjectKind) error {
	if _, err := objectschema.Compile(kind.Group, kind.Kind, kind.Schema); err != nil {
		return fmt.Errorf("schema: %v", err)
	}
	if err := r.rules.CompileRules(kind); err != nil {
		return fmt.Errorf("rules: %v", err)
	}
	return nil
}

// schema returns the compiled schema of a stored kind.
func (r *objectRegistry) schema(kind *structs.ObjectKind) (*objectschema.Schema, error) {
	key := kind.KindRef()
	r.lock.Lock()
	cached, ok := r.schemas[key]
	r.lock.Unlock()
	if ok && cached.modifyIndex == kind.ModifyIndex {
		return cached.schema, cached.err
	}

	schema, err := objectschema.Compile(kind.Group, kind.Kind, kind.Schema)
	r.lock.Lock()
	r.schemas[key] = &compiledObjectSchema{modifyIndex: kind.ModifyIndex, schema: schema, err: err}
	r.lock.Unlock()
	return schema, err
}

// forget drops the cached schema and rules of a kind.
func (r *objectRegistry) forget(kindRef string) {
	r.lock.Lock()
	delete(r.schemas, kindRef)
	r.lock.Unlock()
	r.rules.Forget(kindRef)
}

// objectACLNamespace returns the namespace the ACL rules of an object are
// looked up in: the object's namespace, or "*" for cluster-scoped kinds.
func objectACLNamespace(kind *structs.ObjectKind, namespace string) string {
	if !kind.Namespaced {
		return acl.ObjectClusterNamespace
	}
	return namespace
}

// objectNamespace returns the namespace an object of the kind is stored in.
func objectNamespace(kind *structs.ObjectKind, requested string) string {
	if !kind.Namespaced {
		return ""
	}
	return requested
}

// ObjectKind endpoint manages the type registry of custom objects.
type ObjectKind struct {
	srv *Server
	ctx *RPCContext
}

func NewObjectKindEndpoint(srv *Server, ctx *RPCContext) *ObjectKind {
	return &ObjectKind{srv: srv, ctx: ctx}
}

// List lists the object kinds the caller may read. It supports prefix
// listing ("group/Kind" prefix), pagination and filtering.
func (k *ObjectKind) List(args *structs.ObjectKindListRequest, reply *structs.ObjectKindListResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("ObjectKind.List", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("object_kind", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "object_kind", "list"}, time.Now())

	aclObj, err := k.srv.ResolveACL(args)
	if err != nil {
		return err
	}

	sort := state.SortOption(args.Reverse)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			iter, err := store.ObjectKinds(ws, args.QueryOptions.Prefix, sort)
			if err != nil {
				return err
			}

			aclFilter := paginator.GenericFilter{Allow: func(raw any) (bool, error) {
				return aclObj.AllowObjectKindRead(raw.(*structs.ObjectKind).KindRef()), nil
			}}
			tokenizer := paginator.NewStructsTokenizer(iter, paginator.StructsTokenizerOptions{WithID: true})
			kinds := []*structs.ObjectKind{}
			pager, err := paginator.NewPaginator(iter, tokenizer, []paginator.Filter{aclFilter}, args.QueryOptions,
				func(raw any) error {
					kinds = append(kinds, raw.(*structs.ObjectKind))
					return nil
				})
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to create result paginator: %v", err)
			}
			nextToken, err := pager.Page()
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to read result page: %v", err)
			}

			reply.QueryMeta.NextToken = nextToken
			reply.ObjectKinds = kinds

			index, err := store.Index(state.TableObjectKinds)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			k.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return k.srv.blockingRPC(&opts)
}

// GetObjectKind returns the requested object kind or nil if it doesn't
// exist.
func (k *ObjectKind) GetObjectKind(args *structs.ObjectKindSpecificRequest, reply *structs.SingleObjectKindResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("ObjectKind.GetObjectKind", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("object_kind", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "object_kind", "get_object_kind"}, time.Now())

	aclObj, err := k.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowObjectKindRead(structs.ObjectKindRef(args.Group, args.Kind)) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			kind, err := store.ObjectKindByName(ws, args.Group, args.Kind)
			if err != nil {
				return err
			}
			reply.ObjectKind = kind
			if kind != nil {
				reply.Index = kind.ModifyIndex
			} else {
				index, err := store.Index(state.TableObjectKinds)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			k.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return k.srv.blockingRPC(&opts)
}

// UpsertObjectKind creates or updates an object kind. The schema and the
// rules are compiled first; a kind that doesn't compile is rejected. Objects
// already stored are not validated again against a changed schema.
func (k *ObjectKind) UpsertObjectKind(args *structs.ObjectKindUpsertRequest, reply *structs.ObjectKindUpsertResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("ObjectKind.UpsertObjectKind", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("object_kind", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "object_kind", "upsert_object_kind"}, time.Now())

	aclObj, err := k.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowObjectKindWrite() {
		return structs.ErrPermissionDenied
	}

	if !args.DryRun && !ServersMeetMinimumNorcaVersion(k.srv.serf.Members(), k.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to write object kinds", minNorcaFeaturesVersion)
	}

	kind := args.ObjectKind.Copy()
	if kind == nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "missing object kind")
	}
	kind.CreateIndex, kind.ModifyIndex = 0, 0
	if err := kind.Validate(); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid object kind %q: %v", kind.KindRef(), err)
	}
	if err := k.srv.objects.compile(kind); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid object kind %q: %v", kind.KindRef(), err)
	}

	snap, err := k.srv.State().Snapshot()
	if err != nil {
		return err
	}
	existing, err := snap.ObjectKindByName(nil, kind.Group, kind.Kind)
	if err != nil {
		return err
	}
	if existing != nil && existing.Namespaced != kind.Namespaced {
		iter, err := snap.CustomObjects(nil, kind.Group, kind.Kind, structs.AllNamespacesSentinel, state.SortDefault)
		if err != nil {
			return err
		}
		if iter.Next() != nil {
			return structs.NewErrRPCCodedf(http.StatusConflict,
				"object kind %q has objects; its scope can't change", kind.KindRef())
		}
	}

	if args.DryRun {
		if existing != nil {
			kind.CreateIndex, kind.ModifyIndex = existing.CreateIndex, existing.ModifyIndex
		}
		reply.ObjectKind = kind
		return nil
	}

	req := &structs.ObjectKindUpsertRequest{ObjectKind: kind, WriteRequest: args.WriteRequest}
	_, index, err := k.srv.raftApply(structs.ObjectKindUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	if err != nil {
		return err
	}
	k.srv.objects.forget(kind.KindRef())

	kind.ModifyIndex = index
	kind.CreateIndex = index
	if existing != nil {
		kind.CreateIndex = existing.CreateIndex
	}
	reply.ObjectKind = kind
	reply.Index = index
	return nil
}

// DeleteObjectKind deletes an object kind. A kind that still has objects
// can't be deleted.
func (k *ObjectKind) DeleteObjectKind(args *structs.ObjectKindDeleteRequest, reply *structs.GenericResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("ObjectKind.DeleteObjectKind", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("object_kind", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "object_kind", "delete_object_kind"}, time.Now())

	aclObj, err := k.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowObjectKindWrite() {
		return structs.ErrPermissionDenied
	}

	if !ServersMeetMinimumNorcaVersion(k.srv.serf.Members(), k.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to delete object kinds", minNorcaFeaturesVersion)
	}

	ref := structs.ObjectKindRef(args.Group, args.Kind)
	snap, err := k.srv.State().Snapshot()
	if err != nil {
		return err
	}
	existing, err := snap.ObjectKindByName(nil, args.Group, args.Kind)
	if err != nil {
		return err
	}
	if existing == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", ref)
	}
	iter, err := snap.CustomObjects(nil, args.Group, args.Kind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return err
	}
	if iter.Next() != nil {
		return structs.NewErrRPCCodedf(http.StatusConflict, "object kind %q still has objects; delete them first", ref)
	}

	_, index, err := k.srv.raftApply(structs.ObjectKindDeleteRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	k.srv.objects.forget(ref)
	reply.Index = index
	return nil
}

// CustomObject endpoint reads and writes custom objects.
type CustomObject struct {
	srv *Server
	ctx *RPCContext
}

func NewCustomObjectEndpoint(srv *Server, ctx *RPCContext) *CustomObject {
	return &CustomObject{srv: srv, ctx: ctx}
}

// objectTokenizer returns pagination tokens "namespace,name". The comma
// sorts before every character allowed in namespaces and object names, so
// the tokens sort like the state store iterates the objects of a kind.
type objectTokenizer struct{}

func (objectTokenizer) GetToken(raw any) string {
	obj := raw.(*structs.CustomObject)
	return obj.Namespace + "," + obj.Name
}

// List lists the objects of a kind in a namespace, or in all namespaces with
// the namespace "*". It supports label selection (Labels), filtering and
// pagination.
func (c *CustomObject) List(args *structs.CustomObjectListRequest, reply *structs.CustomObjectListResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.List", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "list"}, time.Now())

	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}

	kindRef := structs.ObjectKindRef(args.Group, args.Kind)
	requested := args.RequestNamespace()
	sort := state.SortOption(args.Reverse)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			kind, err := store.ObjectKindByName(ws, args.Group, args.Kind)
			if err != nil {
				return err
			}
			if kind == nil {
				if !aclObj.AllowObjectOperationInAnyNamespace(kindRef, acl.ObjectCapabilityRead) {
					return structs.ErrPermissionDenied
				}
				return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", kindRef)
			}

			namespace := requested
			if !kind.Namespaced {
				namespace = ""
			}
			allNamespaces := namespace == structs.AllNamespacesSentinel
			switch {
			case allNamespaces && !aclObj.AllowObjectOperationInAnyNamespace(kindRef, acl.ObjectCapabilityRead):
				return structs.ErrPermissionDenied
			case !allNamespaces && !aclObj.AllowObjectOperation(objectACLNamespace(kind, namespace), kindRef, acl.ObjectCapabilityRead):
				return structs.ErrPermissionDenied
			}

			// Use the label index for the first label with a value;
			// the remaining labels and the kind are filtered.
			var iter memdb.ResultIterator
			for key, value := range args.Labels {
				if value != "" {
					iter, err = store.CustomObjectsByLabel(ws, key, value, sort)
					break
				}
			}
			if iter == nil && err == nil {
				iter, err = store.CustomObjects(ws, args.Group, args.Kind, namespace, sort)
			}
			if err != nil {
				return err
			}

			filter := paginator.GenericFilter{Allow: func(raw any) (bool, error) {
				obj := raw.(*structs.CustomObject)
				if obj.Group != args.Group || obj.Kind != args.Kind {
					return false, nil
				}
				if !allNamespaces && obj.Namespace != namespace {
					return false, nil
				}
				for key, value := range args.Labels {
					if v, ok := obj.Labels[key]; !ok || v != value {
						return false, nil
					}
				}
				if allNamespaces && !aclObj.AllowObjectOperation(objectACLNamespace(kind, obj.Namespace), kindRef, acl.ObjectCapabilityRead) {
					return false, nil
				}
				return true, nil
			}}

			objects := []*structs.CustomObject{}
			pager, err := paginator.NewPaginator(iter, objectTokenizer{}, []paginator.Filter{filter}, args.QueryOptions,
				func(raw any) error {
					objects = append(objects, raw.(*structs.CustomObject))
					return nil
				})
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to create result paginator: %v", err)
			}
			nextToken, err := pager.Page()
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to read result page: %v", err)
			}

			reply.QueryMeta.NextToken = nextToken
			reply.Objects = objects

			index, err := store.Index(state.TableCustomObjects)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			c.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return c.srv.blockingRPC(&opts)
}

// GetCustomObject returns the requested object or nil if it doesn't exist.
func (c *CustomObject) GetCustomObject(args *structs.CustomObjectSpecificRequest, reply *structs.SingleCustomObjectResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.GetCustomObject", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "get_custom_object"}, time.Now())

	aclObj, err := c.srv.ResolveACL(args)
	if err != nil {
		return err
	}

	kindRef := structs.ObjectKindRef(args.Group, args.Kind)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			reply.Object = nil
			kind, err := store.ObjectKindByName(ws, args.Group, args.Kind)
			if err != nil {
				return err
			}

			var obj *structs.CustomObject
			if kind == nil {
				if !aclObj.AllowObjectOperation(args.RequestNamespace(), kindRef, acl.ObjectCapabilityRead) {
					return structs.ErrPermissionDenied
				}
			} else {
				namespace := objectNamespace(kind, args.RequestNamespace())
				if !aclObj.AllowObjectOperation(objectACLNamespace(kind, namespace), kindRef, acl.ObjectCapabilityRead) {
					return structs.ErrPermissionDenied
				}
				obj, err = store.CustomObjectByName(ws, args.Group, args.Kind, namespace, args.Name)
				if err != nil {
					return err
				}
			}

			reply.Object = obj
			if obj != nil {
				reply.Index = obj.ModifyIndex
			} else {
				index, err := store.Index(state.TableCustomObjects)
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

// objectWrite describes a write of a custom object for writeObject.
type objectWrite struct {
	args      structs.RequestWithIdentity
	group     string
	kind      string
	namespace string
	name      string
	cas       *uint64
	dryRun    bool

	// capability is the ACL capability the write requires.
	capability string

	// checkManaged enforces the managed_by identity of the stored object.
	checkManaged bool

	// metadataOnly marks writes that change neither the spec nor the status
	// (finalizers, deletion marks). They are not validated against the
	// schema and the rules, so a changed kind can't block the deletion of
	// objects stored before.
	metadataOnly bool

	// build computes the new object from the kind, the namespace the object
	// is stored in and the stored object (nil if it doesn't exist). It
	// returns the CEL operation of the write. aclObj is the ACL of the
	// caller, snap the snapshot the object was read from.
	build func(kind *structs.ObjectKind, namespace string, existing *structs.CustomObject, aclObj *acl.ACL, snap *state.StateSnapshot) (*structs.CustomObject, string, error)
}

// writeObject runs a write of a custom object: it authorizes the write,
// computes the new object from the stored one, fills in defaults, validates
// it against the schema and the CEL rules of its kind, and applies it with
// check-and-set on the modify index it was computed from. Without an
// explicit check-and-set index a conflicting concurrent write is retried.
func (c *CustomObject) writeObject(w *objectWrite, reply *structs.CustomObjectWriteResponse) error {
	aclObj, err := c.srv.ResolveACL(w.args)
	if err != nil {
		return err
	}
	if w.namespace == structs.AllNamespacesSentinel {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "objects can't be written to the namespace %q", w.namespace)
	}
	kindRef := structs.ObjectKindRef(w.group, w.kind)

	for attempt := 1; ; attempt++ {
		snap, err := c.srv.State().Snapshot()
		if err != nil {
			return err
		}
		kind, err := snap.ObjectKindByName(nil, w.group, w.kind)
		if err != nil {
			return err
		}
		if kind == nil {
			if !aclObj.AllowObjectOperation(w.namespace, kindRef, w.capability) {
				return structs.ErrPermissionDenied
			}
			return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", kindRef)
		}
		namespace := objectNamespace(kind, w.namespace)
		if !aclObj.AllowObjectOperation(objectACLNamespace(kind, namespace), kindRef, w.capability) {
			return structs.ErrPermissionDenied
		}
		if kind.Namespaced {
			ns, err := snap.NamespaceByName(nil, namespace)
			if err != nil {
				return err
			}
			if ns == nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "namespace %q not found", namespace)
			}
		}

		existing, err := snap.CustomObjectByName(nil, w.group, w.kind, namespace, w.name)
		if err != nil {
			return err
		}
		if w.cas != nil {
			switch {
			case existing == nil && *w.cas != 0:
				return structs.NewObjectConflictError("%s %q does not exist", kindRef, w.name)
			case existing != nil && existing.ModifyIndex != *w.cas:
				return structs.NewObjectConflictError("%s %q has modify index %d, expected %d",
					kindRef, w.name, existing.ModifyIndex, *w.cas)
			}
		}

		if w.checkManaged && existing != nil {
			if err := checkObjectManagedBy(aclObj, w.args, existing); err != nil {
				return err
			}
		}

		obj, operation, err := w.build(kind, namespace, existing, aclObj, snap)
		if err != nil {
			return err
		}
		if !w.metadataOnly {
			if err := c.validateObject(kind, obj, existing, operation, w); err != nil {
				return err
			}
		}

		if existing != nil && objectUnchanged(existing, obj) {
			reply.Object = existing
			reply.Unchanged = true
			reply.Index = existing.ModifyIndex
			return nil
		}
		if w.dryRun {
			reply.Object = obj
			return nil
		}

		if !ServersMeetMinimumNorcaVersion(c.srv.serf.Members(), c.srv.Region(), false) {
			return fmt.Errorf("all servers must be running Norca %v or later to write custom objects", minNorcaFeaturesVersion)
		}

		var expected uint64
		if existing != nil {
			expected = existing.ModifyIndex
		}
		req := &structs.CustomObjectUpsertStateRequest{
			Object:              obj,
			ExpectedModifyIndex: expected,
			WriteRequest:        structs.WriteRequest{Region: c.srv.Region()},
		}
		_, index, err := c.srv.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
		if err != nil {
			if structs.IsObjectConflictError(err) && w.cas == nil && attempt < objectWriteAttempts {
				// Back off with jitter so that concurrent writers of a
				// hot object don't keep colliding.
				time.Sleep(rand.N(time.Duration(attempt) * objectWriteBackoff))
				continue
			}
			return err
		}

		obj.ModifyIndex = index
		if existing == nil {
			obj.CreateIndex = index
		}
		reply.Object = obj
		reply.Index = index
		return nil
	}
}

// validateObject checks the size of the object and validates it against
// the schema and the CEL rules of its kind.
func (c *CustomObject) validateObject(kind *structs.ObjectKind, obj, existing *structs.CustomObject, operation string, w *objectWrite) error {
	doc := objectDocument(obj)
	size, err := json.Marshal(doc)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "object can't be encoded as JSON: %v", err)
	}
	if len(size) > structs.MaxCustomObjectSize {
		return structs.NewErrRPCCodedf(http.StatusRequestEntityTooLarge,
			"object is larger than %d bytes", structs.MaxCustomObjectSize)
	}

	schema, err := c.srv.objects.schema(kind)
	if err != nil {
		return fmt.Errorf("object kind %q has an invalid schema: %v", kind.KindRef(), err)
	}
	if err := schema.Validate(doc); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%s %q is invalid: %v", kind.KindRef(), obj.Name, err)
	}

	if len(kind.Rules) == 0 {
		return nil
	}
	self, err := admission.ObjectValue(objectCELValue(obj))
	if err != nil {
		return err
	}
	var oldSelf any
	if existing != nil {
		if oldSelf, err = admission.ObjectValue(objectCELValue(existing)); err != nil {
			return err
		}
	}
	user := admissionUser(w.args)
	request := map[string]any{
		"operation": operation,
		"namespace": obj.Namespace,
		"dryRun":    w.dryRun,
		"user": map[string]any{
			"accessor_id": user.AccessorID,
			"name":        user.Name,
			"type":        user.Type,
			"roles":       user.Roles,
			"policies":    user.Policies,
		},
	}
	violations := c.srv.objects.rules.Evaluate(context.Background(), kind, self, oldSelf, request)
	if len(violations) == 0 {
		return nil
	}
	msgs := make([]string, len(violations))
	for i, v := range violations {
		msgs[i] = v.Message
	}
	return structs.NewErrRPCCodedf(http.StatusBadRequest, "%s %q is invalid: %s", kind.KindRef(), obj.Name, strings.Join(msgs, "; "))
}

// objectDocument returns the document the schema of the kind describes.
func objectDocument(obj *structs.CustomObject) map[string]any {
	doc := map[string]any{"spec": obj.Spec}
	if obj.Spec == nil {
		doc["spec"] = map[string]any{}
	}
	if obj.Status != nil {
		doc["status"] = obj.Status
	}
	return doc
}

// objectCELValue returns the object as CEL rules see it in self and
// oldSelf.
func objectCELValue(obj *structs.CustomObject) map[string]any {
	labels := obj.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	finalizers := obj.Finalizers
	if finalizers == nil {
		finalizers = []string{}
	}
	v := map[string]any{
		"kind": obj.KindRef(),
		"metadata": map[string]any{
			"name":        obj.Name,
			"namespace":   obj.Namespace,
			"labels":      labels,
			"managedBy":   obj.ManagedBy,
			"generation":  obj.Generation,
			"createIndex": obj.CreateIndex,
			"modifyIndex": obj.ModifyIndex,
			"finalizers":  finalizers,
			"deleting":    obj.Deleting(),
		},
		"spec": obj.Spec,
	}
	if obj.Spec == nil {
		v["spec"] = map[string]any{}
	}
	if obj.Status != nil {
		v["status"] = obj.Status
	}
	return v
}

// objectUnchanged reports whether a write would leave the stored object as
// it is.
func objectUnchanged(existing, obj *structs.CustomObject) bool {
	return existing.Generation == obj.Generation &&
		existing.ManagedBy == obj.ManagedBy &&
		existing.DeletionTime == obj.DeletionTime &&
		existing.DeletionPropagation == obj.DeletionPropagation &&
		slices.Equal(existing.Finalizers, obj.Finalizers) &&
		reflect.DeepEqual(nonEmptyLabels(existing.Labels), nonEmptyLabels(obj.Labels)) &&
		reflect.DeepEqual(existing.Owners, obj.Owners) &&
		structs.ObjectMapsEqual(existing.Spec, obj.Spec) &&
		structs.ObjectMapsEqual(existing.Status, obj.Status)
}

func nonEmptyLabels(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// normalizeObjectPart converts a spec or status to the stored form.
func normalizeObjectPart(part string, m map[string]any) (map[string]any, error) {
	out, err := structs.NormalizeObjectMap(m)
	if err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%s can't be encoded as JSON: %v", part, err)
	}
	return out, nil
}

// Apply creates or updates the spec and the metadata of an object. Default
// values of the schema are filled in, the generation increases when the
// spec changes, and the status is kept.
func (c *CustomObject) Apply(args *structs.CustomObjectApplyRequest, reply *structs.CustomObjectWriteResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.Apply", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "apply"}, time.Now())

	in := args.Object
	if in == nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "missing object")
	}
	if err := structs.ValidateObjectKindName(in.Group, in.Kind); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	if err := in.ValidateMetadata(); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid object: %v", err)
	}
	spec, err := normalizeObjectPart("spec", in.Spec)
	if err != nil {
		return err
	}
	if spec == nil {
		spec = map[string]any{}
	}

	namespace := args.RequestNamespace()
	w := &objectWrite{
		args:         args,
		group:        in.Group,
		kind:         in.Kind,
		namespace:    namespace,
		name:         in.Name,
		cas:          args.CAS,
		dryRun:       args.DryRun,
		capability:   acl.ObjectCapabilityWrite,
		checkManaged: true,
		build: func(kind *structs.ObjectKind, ns string, existing *structs.CustomObject, aclObj *acl.ACL, snap *state.StateSnapshot) (*structs.CustomObject, string, error) {
			if kind.Namespaced && in.Namespace != "" && in.Namespace != ns {
				return nil, "", structs.NewErrRPCCodedf(http.StatusBadRequest,
					"object namespace %q does not match the request namespace %q", in.Namespace, ns)
			}
			owners, err := resolveObjectOwners(snap, ns, in.Owners)
			if err != nil {
				return nil, "", err
			}
			now := time.Now().UnixNano()
			obj := &structs.CustomObject{
				ID:         uuid.Generate(),
				Group:      kind.Group,
				Kind:       kind.Kind,
				Namespace:  ns,
				Name:       in.Name,
				Labels:     in.Labels,
				ManagedBy:  in.ManagedBy,
				Owners:     owners,
				Generation: 1,
				CreateTime: now,
				ModifyTime: now,
			}
			operation := objectOperationCreate
			if existing != nil {
				operation = objectOperationUpdate
				obj.ID = existing.ID
				obj.Status = structs.CopyObjectMap(existing.Status)
				obj.Finalizers = slices.Clone(existing.Finalizers)
				obj.DeletionTime = existing.DeletionTime
				obj.DeletionPropagation = existing.DeletionPropagation
				obj.Generation = existing.Generation
				obj.CreateTime = existing.CreateTime
				obj.CreateIndex = existing.CreateIndex
				obj.ModifyIndex = existing.ModifyIndex
			}

			// Finalizers of the request are added; removing them takes the
			// finalizers endpoint. Adding one requires manage-finalizers.
			added := false
			for _, f := range in.Finalizers {
				if !slices.Contains(obj.Finalizers, f) {
					obj.Finalizers = append(obj.Finalizers, f)
					added = true
				}
			}
			if added {
				if !aclObj.AllowObjectOperation(objectACLNamespace(kind, ns), kind.KindRef(), acl.ObjectCapabilityManageFinalizers) {
					return nil, "", structs.ErrPermissionDenied
				}
				if obj.Deleting() {
					return nil, "", errObjectDeleting(obj)
				}
			}

			schema, err := c.srv.objects.schema(kind)
			if err != nil {
				return nil, "", fmt.Errorf("object kind %q has an invalid schema: %v", kind.KindRef(), err)
			}
			doc := map[string]any{"spec": structs.CopyObjectMap(spec)}
			schema.ApplyDefaults(doc)
			obj.Spec, _ = doc["spec"].(map[string]any)

			if existing != nil && !structs.ObjectMapsEqual(existing.Spec, obj.Spec) {
				obj.Generation++
			}
			return obj, operation, nil
		},
	}
	return c.writeObject(w, reply)
}

// UpdateStatus replaces the status of an object. The spec, the metadata and
// the generation are not changed.
func (c *CustomObject) UpdateStatus(args *structs.CustomObjectStatusRequest, reply *structs.CustomObjectWriteResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward("CustomObject.UpdateStatus", args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("custom_object", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "custom_object", "update_status"}, time.Now())

	if err := structs.ValidateObjectKindName(args.Group, args.Kind); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	status, err := normalizeObjectPart("status", args.Status)
	if err != nil {
		return err
	}

	w := &objectWrite{
		args:       args,
		group:      args.Group,
		kind:       args.Kind,
		namespace:  args.RequestNamespace(),
		name:       args.Name,
		cas:        args.CAS,
		dryRun:     args.DryRun,
		capability: acl.ObjectCapabilityWriteStatus,
		build: func(kind *structs.ObjectKind, ns string, existing *structs.CustomObject, aclObj *acl.ACL, snap *state.StateSnapshot) (*structs.CustomObject, string, error) {
			if existing == nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", kind.KindRef(), args.Name)
			}
			obj := existing.Copy()
			obj.ModifyTime = time.Now().UnixNano()
			obj.Status = nil
			if status != nil {
				schema, err := c.srv.objects.schema(kind)
				if err != nil {
					return nil, "", fmt.Errorf("object kind %q has an invalid schema: %v", kind.KindRef(), err)
				}
				doc := map[string]any{"spec": structs.CopyObjectMap(existing.Spec), "status": structs.CopyObjectMap(status)}
				schema.ApplyDefaults(doc)
				obj.Status, _ = doc["status"].(map[string]any)
			}
			if err := validateObjectStatus(obj); err != nil {
				return nil, "", structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid status: %v", err)
			}
			return obj, objectOperationStatus, nil
		},
	}
	return c.writeObject(w, reply)
}

// validateObjectStatus checks the status fields with a meaning defined by
// the framework.
func validateObjectStatus(obj *structs.CustomObject) error {
	if raw, ok := obj.Status[structs.ObjectStatusObservedGeneration]; ok {
		g, isNumber := raw.(float64)
		if !isNumber || g < 0 || g != float64(uint64(g)) {
			return fmt.Errorf("%s must be a non-negative integer", structs.ObjectStatusObservedGeneration)
		}
		if uint64(g) > obj.Generation {
			return fmt.Errorf("%s %d is greater than the generation %d",
				structs.ObjectStatusObservedGeneration, uint64(g), obj.Generation)
		}
	}
	if raw, ok := obj.Status[structs.ObjectStatusConditions]; ok {
		conditions, isList := raw.([]any)
		if !isList {
			return fmt.Errorf("%s must be a list", structs.ObjectStatusConditions)
		}
		for i, rawCond := range conditions {
			cond, isMap := rawCond.(map[string]any)
			if !isMap {
				return fmt.Errorf("condition %d must be an object", i)
			}
			if t, ok := cond["type"].(string); !ok || t == "" {
				return fmt.Errorf("condition %d: missing type", i)
			}
			if s, ok := cond["status"].(string); !ok || s == "" {
				return fmt.Errorf("condition %d: missing status", i)
			}
		}
	}
	return nil
}
