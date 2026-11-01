// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net/http"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"

	"github.com/bacanoo-io/norca/nomad/acl"
	"github.com/bacanoo-io/norca/nomad/nomad/state"
	"github.com/bacanoo-io/norca/nomad/nomad/state/paginator"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

// SecretEngine is the RPC endpoint of the dynamic secrets engines of the
// built-in secret store, callable externally through the
// "/v1/secret-engine{s}" HTTP API. Reads need read-secret-engine, writes
// write-secret-engine. Writes run on the leader and are audited.
type SecretEngine struct {
	srv    *Server
	ctx    *RPCContext
	logger hclog.Logger

	// secret provides the ACL resolution and the audit records of the
	// secret store.
	secret *Secret
}

func NewSecretEngineEndpoint(srv *Server, ctx *RPCContext) *SecretEngine {
	return &SecretEngine{srv: srv, ctx: ctx, logger: srv.logger.Named("secret_engines"),
		secret: NewSecretEndpoint(srv, ctx)}
}

// secretEnginePath is the path of the audit records of an engine.
func secretEnginePath(name string) string {
	return "engines/" + name
}

// List lists the engines of a namespace, or of all namespaces with "*".
func (e *SecretEngine) List(args *structs.SecretEngineListRequest, reply *structs.SecretEngineListResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretEngineListRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_engine", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "secret_engine", "list"}, time.Now())

	aclObj, err := e.secret.resolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	allow := func(ns string) bool {
		return aclObj.AllowNsOp(ns, acl.NamespaceCapabilityReadSecretEngine)
	}
	if namespace != structs.AllNamespacesSentinel && !allow(namespace) {
		return structs.ErrPermissionDenied
	}

	return e.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			var iter memdb.ResultIterator
			var err error
			if namespace == structs.AllNamespacesSentinel {
				iter, err = store.SecretEngines(ws)
			} else {
				iter, err = store.SecretEnginesByNamespacePrefix(ws, namespace, args.Prefix)
			}
			if err != nil {
				return err
			}
			tokenizer := paginator.NewStructsTokenizer(iter,
				paginator.StructsTokenizerOptions{WithNamespace: true, WithID: true})
			filters := []paginator.Filter{
				paginator.GenericFilter{
					Allow: func(raw interface{}) (bool, error) {
						engine := raw.(*structs.SecretEngine)
						return strings.HasPrefix(engine.Name, args.Prefix) && allow(engine.Namespace), nil
					},
				},
			}
			stubs := []*structs.SecretEngineStub{}
			pager, err := paginator.NewPaginator(iter, tokenizer, filters, args.QueryOptions,
				func(raw interface{}) error {
					stubs = append(stubs, raw.(*structs.SecretEngine).Stub())
					return nil
				})
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to create result paginator: %v", err)
			}
			nextToken, err := pager.Page()
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to read result page: %v", err)
			}
			reply.Engines = stubs
			reply.NextToken = nextToken
			return e.srv.setReplyQueryMeta(store, state.TableSecretEngines, &reply.QueryMeta)
		},
	})
}

// Read reads an engine. It supports blocking queries.
func (e *SecretEngine) Read(args *structs.SecretEngineReadRequest, reply *structs.SecretEngineResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretEngineReadRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_engine", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "secret_engine", "read"}, time.Now())

	aclObj, err := e.secret.resolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if !aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityReadSecretEngine) {
		return structs.ErrPermissionDenied
	}
	return e.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			engine, err := store.SecretEngineByName(ws, namespace, args.Name)
			if err != nil {
				return err
			}
			reply.Engine = nil
			if engine == nil {
				return e.srv.setReplyQueryMeta(store, state.TableSecretEngines, &reply.QueryMeta)
			}
			reply.Engine = engine.Copy()
			reply.Index = engine.ModifyIndex
			return nil
		},
	})
}

// Upsert creates or updates an engine. The leader reads the engine's admin
// credential, validates the configuration with it and brings the backend in
// line before the engine is written.
func (e *SecretEngine) Upsert(args *structs.SecretEngineUpsertRequest, reply *structs.SecretEngineWriteResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretEngineUpsertRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_engine", structs.RateMetricWrite, args)
	defer metrics.MeasureSince([]string{"nomad", "secret_engine", "upsert"}, time.Now())

	namespace := args.RequestNamespace()
	name := ""
	if args.Engine != nil {
		name = args.Engine.Name
	}
	rec := e.secret.auditRecord(args.GetIdentity(), structs.SecretOpEngineWrite, namespace, secretEnginePath(name))
	rec.Engine = name
	if authErr != nil {
		return e.secret.denied(rec)
	}
	aclObj, err := e.secret.resolveACL(args)
	if err != nil {
		return e.secret.finish(rec, err)
	}
	if !aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityWriteSecretEngine) {
		return e.secret.denied(rec)
	}
	if args.Engine == nil {
		return e.secret.finish(rec, structs.NewErrRPCCoded(http.StatusBadRequest, "missing engine"))
	}
	engine := args.Engine.Copy()
	engine.Namespace = namespace
	engine.Canonicalize()
	if err := engine.Validate(); err != nil {
		return e.secret.finish(rec, structs.NewErrRPCCoded(http.StatusBadRequest, err.Error()))
	}
	if err := e.validateNamespace(namespace); err != nil {
		return e.secret.finish(rec, err)
	}

	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	out, index, err := e.srv.secretEngineUpsert(engine, args.CheckIndex)
	if err := e.secret.finish(rec, err); err != nil {
		return err
	}
	reply.Engine = out
	reply.Index = index
	return nil
}

// Delete deletes an engine that has no live leases.
func (e *SecretEngine) Delete(args *structs.SecretEngineDeleteRequest, reply *structs.SecretEngineWriteResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretEngineDeleteRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_engine", structs.RateMetricWrite, args)
	defer metrics.MeasureSince([]string{"nomad", "secret_engine", "delete"}, time.Now())

	namespace := args.RequestNamespace()
	rec := e.secret.auditRecord(args.GetIdentity(), structs.SecretOpEngineDelete, namespace, secretEnginePath(args.Name))
	rec.Engine = args.Name
	if authErr != nil {
		return e.secret.denied(rec)
	}
	aclObj, err := e.secret.resolveACL(args)
	if err != nil {
		return e.secret.finish(rec, err)
	}
	if !aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityWriteSecretEngine) {
		return e.secret.denied(rec)
	}
	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	index, err := e.srv.secretEngineDelete(namespace, args.Name, args.CheckIndex)
	if err := e.secret.finish(rec, err); err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// validateNamespace checks the version gate and the namespace of a write.
func (e *SecretEngine) validateNamespace(namespace string) error {
	if !ServersMeetMinimumNorcaVersion(e.srv.Members(), e.srv.Region(), false) {
		return structs.NewErrRPCCoded(http.StatusServiceUnavailable,
			"all servers in the region must run a Norca version that supports the secret store")
	}
	if namespace == structs.AllNamespacesSentinel {
		return structs.NewErrRPCCoded(http.StatusBadRequest, `can not write to the wildcard ("*") namespace`)
	}
	ns, err := e.srv.State().NamespaceByName(nil, namespace)
	if err != nil {
		return err
	}
	if ns == nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "namespace %q does not exist", namespace)
	}
	return nil
}

// SecretLease is the RPC endpoint of the leases of the dynamic secrets.
// Tasks create, renew and revoke their own leases with their workload
// identity; operators list and read leases with read-secret-engine and
// revoke them with write-secret-engine.
type SecretLease struct {
	srv    *Server
	ctx    *RPCContext
	logger hclog.Logger
	secret *Secret
}

func NewSecretLeaseEndpoint(srv *Server, ctx *RPCContext) *SecretLease {
	return &SecretLease{srv: srv, ctx: ctx, logger: srv.logger.Named("secret_leases"),
		secret: NewSecretEndpoint(srv, ctx)}
}

// List lists the leases of a namespace (or of all namespaces with "*"),
// optionally of one engine.
func (e *SecretLease) List(args *structs.SecretLeaseListRequest, reply *structs.SecretLeaseListResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretLeaseListRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_lease", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "secret_lease", "list"}, time.Now())

	aclObj, err := e.secret.resolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	allow := func(ns string) bool {
		return aclObj.AllowNsOp(ns, acl.NamespaceCapabilityReadSecretEngine)
	}
	if namespace != structs.AllNamespacesSentinel && !allow(namespace) {
		return structs.ErrPermissionDenied
	}

	return e.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			// The leases are listed in the order of their IDs, which the
			// pagination tokens follow.
			iter, err := store.SecretLeases(ws)
			if err != nil {
				return err
			}
			tokenizer := paginator.NewStructsTokenizer(iter,
				paginator.StructsTokenizerOptions{WithID: true})
			filters := []paginator.Filter{
				paginator.GenericFilter{
					Allow: func(raw interface{}) (bool, error) {
						l := raw.(*structs.SecretLease)
						return (namespace == structs.AllNamespacesSentinel || l.Namespace == namespace) &&
							(args.Engine == "" || l.Engine == args.Engine) &&
							strings.HasPrefix(l.ID, args.Prefix) && allow(l.Namespace), nil
					},
				},
			}
			leases := []*structs.SecretLease{}
			pager, err := paginator.NewPaginator(iter, tokenizer, filters, args.QueryOptions,
				func(raw interface{}) error {
					leases = append(leases, raw.(*structs.SecretLease).Copy())
					return nil
				})
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to create result paginator: %v", err)
			}
			nextToken, err := pager.Page()
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to read result page: %v", err)
			}
			reply.Leases = leases
			reply.NextToken = nextToken
			return e.srv.setReplyQueryMeta(store, state.TableSecretLeases, &reply.QueryMeta)
		},
	})
}

// Read reads a lease.
func (e *SecretLease) Read(args *structs.SecretLeaseReadRequest, reply *structs.SecretLeaseResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretLeaseReadRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_lease", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "secret_lease", "read"}, time.Now())

	aclObj, err := e.secret.resolveACL(args)
	if err != nil {
		return err
	}
	return e.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			lease, err := store.SecretLeaseByID(ws, args.LeaseID)
			if err != nil {
				return err
			}
			reply.Lease = nil
			if lease == nil {
				return e.srv.setReplyQueryMeta(store, state.TableSecretLeases, &reply.QueryMeta)
			}
			if !aclObj.AllowNsOp(lease.Namespace, acl.NamespaceCapabilityReadSecretEngine) {
				return structs.ErrPermissionDenied
			}
			reply.Lease = lease.Copy()
			reply.Index = lease.ModifyIndex
			return nil
		},
	})
}

// leaseClaims returns the workload identity of a request. Without ACLs the
// servers don't resolve identities, but a lease still belongs to a task: the
// token is verified as a workload identity here.
func (e *SecretLease) leaseClaims(args structs.RequestWithIdentity) *structs.IdentityClaims {
	if claims := args.GetIdentity().GetClaims(); claims != nil {
		return claims
	}
	if e.srv.config.ACLEnabled {
		return nil
	}
	token := args.GetAuthToken()
	if token == "" {
		return nil
	}
	claims, err := e.srv.VerifyClaim(token)
	if err != nil {
		return nil
	}
	return claims
}

// leaseAlloc returns the running allocation and task of a workload identity.
func (e *SecretLease) leaseAlloc(args structs.RequestWithIdentity) (*structs.Allocation, string, error) {
	claims := e.leaseClaims(args)
	if claims == nil {
		return nil, "", structs.NewErrRPCCoded(http.StatusForbidden, "secret leases require a workload identity")
	}
	alloc, err := e.srv.State().AllocByID(nil, claims.AllocationID)
	if err != nil {
		return nil, "", err
	}
	if alloc == nil || alloc.Job == nil || alloc.TerminalStatus() {
		return nil, "", structs.ErrPermissionDenied
	}
	return alloc, claims.TaskName, nil
}

// Create creates a credential of a role for the task of the request's
// workload identity. The task must have a secret block naming the engine and
// the role, and read-secret on the credentials path of the role, from a
// policy bound to its job or a secret_access binding.
func (e *SecretLease) Create(args *structs.SecretLeaseCreateRequest, reply *structs.SecretLeaseCreateResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretLeaseCreateRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_lease", structs.RateMetricWrite, args)
	defer metrics.MeasureSince([]string{"nomad", "secret_lease", "create"}, time.Now())

	namespace := args.RequestNamespace()
	path := structs.SecretEngineCredsPath(args.Engine, args.Role)
	rec := e.secret.auditRecord(args.GetIdentity(), structs.SecretOpLeaseCreate, namespace, path)
	rec.Engine, rec.Role = args.Engine, args.Role
	if authErr != nil {
		return e.secret.denied(rec)
	}
	if err := structs.ValidateSecretEngineName("engine", args.Engine); err != nil {
		return e.secret.finish(rec, structs.NewErrRPCCoded(http.StatusBadRequest, err.Error()))
	}
	if err := structs.ValidateSecretEngineName("role", args.Role); err != nil {
		return e.secret.finish(rec, structs.NewErrRPCCoded(http.StatusBadRequest, err.Error()))
	}
	alloc, task, err := e.leaseAlloc(args)
	if err != nil {
		return e.secret.finish(rec, err)
	}
	rec.Namespace = alloc.Namespace
	if !taskRequestsEngine(alloc, task, args) {
		return e.secret.finish(rec, structs.NewErrRPCCodedf(http.StatusForbidden,
			"task %q has no secret block %q with engine %q and role %q", task, args.SecretName, args.Engine, args.Role))
	}
	aclObj, err := e.secret.resolveACL(args)
	if err != nil {
		return e.secret.finish(rec, err)
	}
	if !e.secret.allowed(aclObj, args, alloc.Namespace, path, acl.NamespaceCapabilityReadSecret) {
		return e.secret.denied(rec)
	}

	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	lease, cred, index, err := e.srv.secretLeaseCreate(alloc, task, args)
	if lease != nil {
		rec.LeaseID = lease.ID
	}
	if err := e.secret.finish(rec, err); err != nil {
		return err
	}
	metrics.IncrCounterWithLabels([]string{"nomad", "secrets", "leases", "created"}, 1,
		[]metrics.Label{{Name: "engine", Value: args.Engine}})
	reply.Lease = lease
	reply.Items = cred.Items
	reply.Index = index
	return nil
}

// taskRequestsEngine reports whether the task of the allocation has the
// secret block of the request.
func taskRequestsEngine(alloc *structs.Allocation, task string, args *structs.SecretLeaseCreateRequest) bool {
	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		return false
	}
	t := tg.LookupTask(task)
	if t == nil {
		return false
	}
	for _, s := range t.Secrets {
		if s != nil && s.Name == args.SecretName && s.Engine == args.Engine && s.Role == args.Role {
			return true
		}
	}
	return false
}

// Renew extends a lease of the request's task by its TTL, up to its max_ttl.
func (e *SecretLease) Renew(args *structs.SecretLeaseRenewRequest, reply *structs.SecretLeaseWriteResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretLeaseRenewRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_lease", structs.RateMetricWrite, args)
	defer metrics.MeasureSince([]string{"nomad", "secret_lease", "renew"}, time.Now())

	rec := e.secret.auditRecord(args.GetIdentity(), structs.SecretOpLeaseRenew, args.RequestNamespace(), "")
	rec.LeaseID = args.LeaseID
	if authErr != nil {
		return e.secret.denied(rec)
	}
	alloc, task, err := e.leaseAlloc(args)
	if err != nil {
		return e.secret.finish(rec, err)
	}
	lease, err := e.srv.State().SecretLeaseByID(nil, args.LeaseID)
	if err != nil {
		return e.secret.finish(rec, err)
	}
	if lease == nil {
		return e.secret.finish(rec, structs.ErrSecretLeaseNotFound)
	}
	rec.Namespace, rec.Path, rec.Engine, rec.Role = lease.Namespace, lease.CredsPath(), lease.Engine, lease.Role
	if lease.AllocID != alloc.ID || lease.Task != task {
		return e.secret.denied(rec)
	}
	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	out, items, index, err := e.srv.secretLeaseRenew(lease)
	if err := e.secret.finish(rec, err); err != nil {
		return err
	}
	metrics.IncrCounterWithLabels([]string{"nomad", "secrets", "leases", "renewed"}, 1,
		[]metrics.Label{{Name: "engine", Value: lease.Engine}})
	reply.Lease = out
	reply.Items = items
	reply.Index = index
	return nil
}

// Revoke revokes a lease or, for operators, every live lease of an engine.
// A task may revoke its own leases (when it stops); operators need
// write-secret-engine.
func (e *SecretLease) Revoke(args *structs.SecretLeaseRevokeRequest, reply *structs.SecretLeaseRevokeResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretLeaseRevokeRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret_lease", structs.RateMetricWrite, args)
	defer metrics.MeasureSince([]string{"nomad", "secret_lease", "revoke"}, time.Now())

	namespace := args.RequestNamespace()
	rec := e.secret.auditRecord(args.GetIdentity(), structs.SecretOpLeaseRevoke, namespace, "")
	rec.LeaseID, rec.Engine = args.LeaseID, args.Engine
	if authErr != nil {
		return e.secret.denied(rec)
	}
	if (args.LeaseID == "") == (args.Engine == "") {
		return e.secret.finish(rec, structs.NewErrRPCCoded(http.StatusBadRequest, "revoke requires either a lease ID or an engine"))
	}

	now := time.Now()
	var leases []*structs.SecretLease
	reason := structs.SecretLeaseRevokeOperator
	if claims := e.leaseClaims(args); claims != nil {
		// A task revokes its own lease.
		if args.LeaseID == "" {
			return e.secret.denied(rec)
		}
		lease, err := e.srv.State().SecretLeaseByID(nil, args.LeaseID)
		if err != nil {
			return e.secret.finish(rec, err)
		}
		if lease == nil {
			return e.secret.finish(rec, structs.ErrSecretLeaseNotFound)
		}
		rec.Namespace, rec.Path, rec.Engine, rec.Role = lease.Namespace, lease.CredsPath(), lease.Engine, lease.Role
		if lease.AllocID != claims.AllocationID || lease.Task != claims.TaskName {
			return e.secret.denied(rec)
		}
		reason = structs.SecretLeaseRevokeStopped
		leases = append(leases, lease)
	} else {
		aclObj, err := e.secret.resolveACL(args)
		if err != nil {
			return e.secret.finish(rec, err)
		}
		if args.LeaseID != "" {
			lease, err := e.srv.State().SecretLeaseByID(nil, args.LeaseID)
			if err != nil {
				return e.secret.finish(rec, err)
			}
			if lease == nil {
				return e.secret.finish(rec, structs.ErrSecretLeaseNotFound)
			}
			rec.Namespace, rec.Path, rec.Engine, rec.Role = lease.Namespace, lease.CredsPath(), lease.Engine, lease.Role
			if !aclObj.AllowNsOp(lease.Namespace, acl.NamespaceCapabilityWriteSecretEngine) {
				return e.secret.denied(rec)
			}
			leases = append(leases, lease)
		} else {
			rec.Path = secretEnginePath(args.Engine)
			if !aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityWriteSecretEngine) {
				return e.secret.denied(rec)
			}
			iter, err := e.srv.State().SecretLeasesByEngine(nil, namespace, args.Engine)
			if err != nil {
				return e.secret.finish(rec, err)
			}
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				leases = append(leases, raw.(*structs.SecretLease))
			}
		}
	}

	var revoked []*structs.SecretLease
	for _, l := range leases {
		if !l.Revoked {
			revoked = append(revoked, secretLeaseRevoked(l, reason, now))
		}
	}
	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	var index uint64
	var err error
	if len(revoked) > 0 {
		index, err = e.srv.secretLeaseApply(revoked)
	}
	rec.Reason = reason
	if err := e.secret.finish(rec, err); err != nil {
		return err
	}
	reply.Revoked = []string{}
	for _, l := range revoked {
		reply.Revoked = append(reply.Revoked, l.ID)
		metrics.IncrCounterWithLabels([]string{"nomad", "secrets", "leases", "revoked"}, 1,
			[]metrics.Label{{Name: "engine", Value: l.Engine}, {Name: "reason", Value: reason}})
	}
	reply.Index = index
	return nil
}
