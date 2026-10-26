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

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/state/paginator"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Secret is the RPC endpoint of the secrets of the built-in secret store,
// callable externally through the "/v1/secret{s}" HTTP API.
//
// Metadata reads (List, ReadMetadata) come from the state store and may be
// served by followers when stale reads are allowed. Value reads and writes
// are always executed by the leader, on its unsealed uBixVault core, and
// each of them is audited. Access is decided by the secret capabilities of
// the namespace; workload identities get access only through policies bound
// to their job, there is no implicit access.
type Secret struct {
	srv    *Server
	ctx    *RPCContext
	logger hclog.Logger
}

func NewSecretEndpoint(srv *Server, ctx *RPCContext) *Secret {
	return &Secret{srv: srv, ctx: ctx, logger: srv.logger.Named("secrets")}
}

// List lists the metadata of the secrets of a namespace, or of all
// namespaces with "*", whose path starts with the prefix.
func (e *Secret) List(args *structs.SecretListRequest, reply *structs.SecretListResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretListRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "secret", "list"}, time.Now())

	aclObj, err := e.resolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if !aclObj.AllowSecretSearch(namespace) {
		return structs.ErrPermissionDenied
	}

	return e.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			var iter memdb.ResultIterator
			var err error
			if namespace == structs.AllNamespacesSentinel {
				iter, err = store.Secrets(ws)
			} else {
				iter, err = store.SecretsByNamespacePrefix(ws, namespace, args.Prefix)
			}
			if err != nil {
				return err
			}

			tokenizer := paginator.NewStructsTokenizer(iter,
				paginator.StructsTokenizerOptions{WithNamespace: true, WithID: true})
			filters := []paginator.Filter{
				paginator.GenericFilter{
					Allow: func(raw interface{}) (bool, error) {
						secret := raw.(*structs.Secret)
						if !strings.HasPrefix(secret.Path, args.Prefix) {
							return false, nil
						}
						return aclObj.AllowSecretOperation(secret.Namespace, secret.Path,
							acl.NamespaceCapabilityListSecrets), nil
					},
				},
			}

			stubs := []*structs.SecretStub{}
			pager, err := paginator.NewPaginator(iter, tokenizer, filters, args.QueryOptions,
				func(raw interface{}) error {
					stubs = append(stubs, raw.(*structs.Secret).Stub())
					return nil
				})
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to create result paginator: %v", err)
			}
			nextToken, err := pager.Page()
			if err != nil {
				return structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to read result page: %v", err)
			}

			reply.Secrets = stubs
			reply.NextToken = nextToken
			return e.srv.setReplyQueryMeta(store, state.TableSecrets, &reply.QueryMeta)
		},
	})
}

// ReadMetadata reads the metadata of a secret, never its value. It supports
// blocking queries.
func (e *Secret) ReadMetadata(args *structs.SecretReadMetadataRequest, reply *structs.SecretMetadataResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretReadMetadataRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "secret", "read_metadata"}, time.Now())

	aclObj, err := e.resolveACL(args)
	if err != nil {
		return err
	}
	namespace := args.RequestNamespace()
	if !e.allowed(aclObj, args, namespace, args.Path, acl.NamespaceCapabilityListSecrets) {
		return structs.ErrPermissionDenied
	}
	return e.readMetadata(namespace, args.Path, &args.QueryOptions, &reply.QueryMeta, &reply.Secret)
}

// readMetadata runs the blocking query of a secret's metadata row.
func (e *Secret) readMetadata(namespace, path string, opts *structs.QueryOptions, meta *structs.QueryMeta, out **structs.Secret) error {
	return e.srv.blockingRPC(&blockingOptions{
		queryOpts: opts,
		queryMeta: meta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			secret, err := store.SecretByPath(ws, namespace, path)
			if err != nil {
				return err
			}
			*out = nil
			if secret == nil {
				return e.srv.setReplyQueryMeta(store, state.TableSecrets, meta)
			}
			*out = secret.Copy()
			meta.Index = secret.ModifyIndex
			return nil
		},
	})
}

// Read reads the value of a version of a secret on the leader's core. The
// query blocks on the secret's metadata; the value is read once the query
// returns. Every read, also a denied one, produces an audit record.
func (e *Secret) Read(args *structs.SecretReadRequest, reply *structs.SecretReadResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	// Values are only read by the leader's core.
	args.AllowStale = false
	if done, err := e.srv.forward(structs.SecretReadRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricRead, args)
	defer metrics.MeasureSince([]string{"nomad", "secret", "read"}, time.Now())

	namespace := args.RequestNamespace()
	rec := e.auditRecord(args.GetIdentity(), structs.SecretOpRead, namespace, args.Path)
	rec.Version = args.Version
	if authErr != nil {
		return e.denied(rec)
	}
	aclObj, err := e.resolveACL(args)
	if err != nil {
		return e.finish(rec, err)
	}
	if !e.allowed(aclObj, args, namespace, args.Path, acl.NamespaceCapabilityReadSecret) {
		return e.denied(rec)
	}

	var secret *structs.Secret
	if err := e.readMetadata(namespace, args.Path, &args.QueryOptions, &reply.QueryMeta, &secret); err != nil {
		return e.finish(rec, err)
	}
	if secret == nil {
		// No secret at the path: an empty response, as for other reads.
		return e.finish(rec, structs.ErrSecretNotFound, structs.ErrSecretNotFound)
	}

	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	items, version, err := e.srv.secretReadValue(namespace, args.Path, args.Version)
	rec.Version = version
	if err := e.finish(rec, err); err != nil {
		return err
	}
	reply.Secret = secret
	reply.Version = version
	reply.Items = items
	return nil
}

// Upsert writes a new version of a secret and/or its metadata.
func (e *Secret) Upsert(args *structs.SecretUpsertRequest, reply *structs.SecretWriteResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretUpsertRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricWrite, args)
	defer metrics.MeasureSince([]string{"nomad", "secret", "upsert"}, time.Now())

	namespace := args.RequestNamespace()
	rec := e.auditRecord(args.GetIdentity(), structs.SecretOpWrite, namespace, args.Path)
	if authErr != nil {
		return e.denied(rec)
	}
	aclObj, err := e.resolveACL(args)
	if err != nil {
		return e.finish(rec, err)
	}
	if !aclObj.AllowSecretOperation(namespace, args.Path, acl.NamespaceCapabilityWriteSecret) {
		return e.denied(rec)
	}
	if err := e.validateWrite(namespace, args.Path); err != nil {
		return e.finish(rec, err)
	}
	if len(args.Items) > 0 {
		if err := args.Items.Validate(); err != nil {
			return e.finish(rec, structs.NewErrRPCCoded(http.StatusBadRequest, err.Error()))
		}
	}
	if args.Meta != nil {
		if err := args.Meta.Validate(); err != nil {
			return e.finish(rec, structs.NewErrRPCCoded(http.StatusBadRequest, err.Error()))
		}
	}

	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	secret, index, err := e.srv.secretUpsert(args)
	if secret != nil && len(args.Items) > 0 {
		rec.Version = secret.CurrentVersion
	}
	if err := e.finish(rec, err); err != nil {
		return err
	}
	reply.Secret = secret
	reply.Index = index
	return nil
}

// Delete soft deletes versions of a secret (the current one by default).
func (e *Secret) Delete(args *structs.SecretVersionsRequest, reply *structs.SecretWriteResponse) error {
	return e.versionsOp(structs.SecretDeleteRPCMethod, structs.SecretOpDelete,
		acl.NamespaceCapabilityWriteSecret, args, reply)
}

// Undelete restores soft deleted versions of a secret (the current one by
// default).
func (e *Secret) Undelete(args *structs.SecretVersionsRequest, reply *structs.SecretWriteResponse) error {
	return e.versionsOp(structs.SecretUndeleteRPCMethod, structs.SecretOpUndelete,
		acl.NamespaceCapabilityWriteSecret, args, reply)
}

// Destroy destroys versions of a secret permanently, or the whole secret
// with all of its versions when no version is given.
func (e *Secret) Destroy(args *structs.SecretVersionsRequest, reply *structs.SecretWriteResponse) error {
	return e.versionsOp(structs.SecretDestroyRPCMethod, structs.SecretOpDestroy,
		acl.NamespaceCapabilityDestroySecret, args, reply)
}

func (e *Secret) versionsOp(method, op, capability string, args *structs.SecretVersionsRequest, reply *structs.SecretWriteResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(method, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricWrite, args)
	defer metrics.MeasureSince([]string{"nomad", "secret", op}, time.Now())

	namespace := args.RequestNamespace()
	rec := e.auditRecord(args.GetIdentity(), op, namespace, args.Path)
	rec.Versions = append([]uint64(nil), args.Versions...)
	if authErr != nil {
		return e.denied(rec)
	}
	aclObj, err := e.resolveACL(args)
	if err != nil {
		return e.finish(rec, err)
	}
	if !aclObj.AllowSecretOperation(namespace, args.Path, capability) {
		return e.denied(rec)
	}
	if err := e.validateWrite(namespace, args.Path); err != nil {
		return e.finish(rec, err)
	}

	if err := e.srv.secretAudit(rec, false); err != nil {
		return err
	}
	secret, index, err := e.srv.secretVersionsOp(op, args)
	if err := e.finish(rec, err); err != nil {
		return err
	}
	reply.Secret = secret
	reply.Index = index
	return nil
}

// validateWrite checks the version gate and the target of a write.
func (e *Secret) validateWrite(namespace, path string) error {
	if !ServersMeetMinimumNorcaVersion(e.srv.Members(), e.srv.Region(), false) {
		return structs.NewErrRPCCoded(http.StatusServiceUnavailable,
			"all servers in the region must run a Norca version that supports the secret store")
	}
	if namespace == structs.AllNamespacesSentinel {
		return structs.NewErrRPCCoded(http.StatusBadRequest, `can not write to the wildcard ("*") namespace`)
	}
	if err := structs.ValidateSecretPath(path); err != nil {
		return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
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

// resolveACL resolves the ACL object of the request's ACL token or workload
// identity. A workload identity is granted only the policies bound to its
// job, and nothing once its allocation is terminal.
func (e *Secret) resolveACL(args structs.RequestWithIdentity) (*acl.ACL, error) {
	claims := args.GetIdentity().GetClaims()
	if claims != nil && e.srv.config.ACLEnabled {
		alloc, err := e.srv.State().AllocByID(nil, claims.AllocationID)
		if err != nil {
			return nil, err
		}
		if alloc == nil || alloc.Job == nil || alloc.TerminalStatus() {
			return nil, structs.ErrPermissionDenied
		}
	}
	aclObj, err := e.srv.ResolveACL(args)
	if err != nil && claims != nil {
		// Claims of an allocation that no longer exists.
		return nil, structs.ErrPermissionDenied
	}
	return aclObj, err
}

// RecordAccess records the audit record of a fetch a client made from an
// external provider for a task (Norca, phase 3). The request is
// authenticated with the task's workload identity, which is the accessor of
// the record; the client supplies only the provider, path, version and
// result. Fetches from the built-in store are recorded by Read itself and
// are not accepted here.
func (e *Secret) RecordAccess(args *structs.SecretRecordAccessRequest, reply *structs.GenericResponse) error {
	authErr := e.srv.Authenticate(e.ctx, args)
	if done, err := e.srv.forward(structs.SecretRecordAccessRPCMethod, args, args, reply); done {
		return err
	}
	e.srv.MeasureRPCRate("secret", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	claims := args.GetIdentity().GetClaims()
	if claims == nil {
		return structs.NewErrRPCCoded(http.StatusForbidden, "secret access records require a workload identity")
	}
	alloc, err := e.srv.State().AllocByID(nil, claims.AllocationID)
	if err != nil {
		return err
	}
	if alloc == nil || alloc.Job == nil || alloc.ClientTerminalStatus() {
		return structs.ErrPermissionDenied
	}

	in := args.Record
	switch {
	case in == nil:
		return structs.NewErrRPCCoded(http.StatusBadRequest, "missing record")
	case in.Provider == "" || in.Provider == structs.SecretProviderNorca:
		return structs.NewErrRPCCoded(http.StatusBadRequest, "records are only accepted for external providers")
	case in.Path == "" || len(in.Path) > 512 || len(in.Provider) > 64 || len(in.ProviderVersion) > 128:
		return structs.NewErrRPCCoded(http.StatusBadRequest, "invalid record")
	}
	rec := e.auditRecord(args.GetIdentity(), structs.SecretOpRead, alloc.Namespace, in.Path)
	rec.Provider = in.Provider
	rec.ProviderVersion = in.ProviderVersion
	switch in.Result {
	case structs.SecretAccessResultOK, structs.SecretAccessResultDenied, structs.SecretAccessResultNotFound,
		structs.SecretAccessResultSealed, structs.SecretAccessResultInvalid:
		rec.Result = in.Result
	default:
		rec.Result = structs.SecretAccessResultError
	}
	if rec.Result != structs.SecretAccessResultOK {
		rec.Error = in.Error
		if len(rec.Error) > 1024 {
			rec.Error = rec.Error[:1024]
		}
	}
	return e.srv.secretAudit(rec, true)
}

// allowed checks a secret capability: granted by the ACL object, or, for
// read-secret and list-secrets of a workload identity, by the secret_access
// bindings of its allocation's job (Norca, phase 3). The submitter of the
// job was authorized for the bindings at registration.
func (e *Secret) allowed(aclObj *acl.ACL, args structs.RequestWithIdentity, namespace, path, capability string) bool {
	if aclObj.AllowSecretOperation(namespace, path, capability) {
		return true
	}
	claims := args.GetIdentity().GetClaims()
	if claims == nil {
		return false
	}
	alloc, err := e.srv.State().AllocByID(nil, claims.AllocationID)
	if err != nil || alloc == nil || alloc.TerminalStatus() {
		return false
	}
	return secretAccessGranted(alloc, claims, namespace, path, capability)
}

// auditRecord starts the audit record of an access.
func (e *Secret) auditRecord(identity *structs.AuthenticatedIdentity, op, namespace, path string) *structs.SecretAccessEvent {
	return &structs.SecretAccessEvent{
		Region:    e.srv.Region(),
		Server:    e.srv.config.NodeName,
		Namespace: namespace,
		Path:      path,
		Operation: op,
		Accessor:  e.srv.secretAccessor(identity),
	}
}

// denied records a denied access and returns the permission error.
func (e *Secret) denied(rec *structs.SecretAccessEvent) error {
	return e.finish(rec, structs.ErrPermissionDenied)
}

// finish records the result of an access and returns the error the RPC
// returns: the coded form of err, or of ret if given, or the audit error if
// the record of a successful access could not be written.
func (e *Secret) finish(rec *structs.SecretAccessEvent, err error, ret ...error) error {
	rec.Result = secretAccessResult(err)
	if err != nil {
		rec.Error = err.Error()
	}
	auditErr := e.srv.secretAudit(rec, true)
	switch {
	case len(ret) > 0:
		if errorIsSecretNotFound(ret[0]) {
			return auditErr
		}
		return secretCodedError(ret[0])
	case err != nil:
		if err == structs.ErrPermissionDenied {
			return err
		}
		return secretCodedError(err)
	default:
		return auditErr
	}
}

// errorIsSecretNotFound reports a missing secret, which a read returns as an
// empty response rather than an error.
func errorIsSecretNotFound(err error) bool {
	return err == structs.ErrSecretNotFound
}

// secretAccessor describes the accessor of a request for the audit trail.
func (s *Server) secretAccessor(identity *structs.AuthenticatedIdentity) *structs.SecretAccessor {
	a := &structs.SecretAccessor{}
	if identity == nil {
		return a
	}
	if token := identity.GetACLToken(); token != nil {
		a.AccessorID = token.AccessorID
	}
	if claims := identity.GetClaims(); claims != nil {
		a.Namespace = claims.Namespace
		a.JobID = claims.JobID
		a.Task = claims.TaskName
		a.AllocationID = claims.AllocationID
		if alloc, err := s.State().AllocByID(nil, claims.AllocationID); err == nil && alloc != nil {
			a.TaskGroup = alloc.TaskGroup
			a.NodeID = alloc.NodeID
		}
	}
	if identity.ClientID != "" {
		a.NodeID = identity.ClientID
	}
	if identity.RemoteIP != nil {
		a.RemoteIP = identity.RemoteIP.String()
	}
	return a
}
