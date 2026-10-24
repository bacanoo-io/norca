// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Release endpoint installs, upgrades, rolls back, tests and uninstalls pack
// releases, and reads them. Applying a release grants no permissions: every
// resource is written with the token of the caller through the endpoint of
// its type. The release objects themselves are authorized with the object
// rules of the packs.norca.io/Release kind.
type Release struct {
	srv *Server
	ctx *RPCContext
}

func NewReleaseEndpoint(srv *Server, ctx *RPCContext) *Release {
	return &Release{srv: srv, ctx: ctx}
}

// authorize resolves the ACL of a request and checks an object capability
// on releases in the namespace.
func (r *Release) authorize(args structs.RequestWithIdentity, namespace, capability string) (*acl.ACL, error) {
	aclObj, err := r.srv.ResolveACL(args)
	if err != nil {
		return nil, err
	}
	if namespace == structs.AllNamespacesSentinel {
		if !aclObj.AllowObjectOperationInAnyNamespace(structs.ReleaseKindRef, capability) {
			return nil, structs.ErrPermissionDenied
		}
		return aclObj, nil
	}
	if !aclObj.AllowObjectOperation(namespace, structs.ReleaseKindRef, capability) {
		return nil, structs.ErrPermissionDenied
	}
	return aclObj, nil
}

// prepareWrite runs the checks common to the operations that change a
// release.
func (r *Release) prepareWrite(name string, dryRun bool) error {
	if err := structs.ValidateReleaseName(name); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	if !dryRun && !ServersMeetMinimumNorcaVersion(r.srv.serf.Members(), r.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to use releases", minNorcaFeaturesVersion)
	}
	if err := r.srv.releases.ensureKinds(); err != nil && !dryRun {
		return err
	}
	return nil
}

func validateTimeout(d time.Duration) error {
	if d < 0 || d > structs.MaxReleaseTimeout {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "timeout must be between 0 and %s", structs.MaxReleaseTimeout)
	}
	return nil
}

// Apply installs or upgrades a release, or plans it with DryRun.
func (r *Release) Apply(args *structs.ReleaseApplyRequest, reply *structs.ReleaseApplyResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.Apply", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "apply"}, time.Now())

	namespace := args.RequestNamespace()
	if _, err := r.authorize(args, namespace, acl.ObjectCapabilityWrite); err != nil {
		return err
	}
	if err := r.prepareWrite(args.Name, args.DryRun); err != nil {
		return err
	}
	if err := validateTimeout(args.Timeout); err != nil {
		return err
	}
	if args.MaxHistory < 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "max_history can't be negative")
	}
	if args.DryRun && args.RecordOnly {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "dry_run and record_only are exclusive")
	}
	secrets, err := secretsByPath(args.Secrets, namespace)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	if err := checkSecretsHaveVariables(secrets, args.Resources, namespace); err != nil {
		return err
	}

	op, cancel := r.srv.releases.newOp(args, args.AuthToken, namespace, args.Name, args.Timeout)
	defer cancel()
	in := &applyInput{
		pack:          args.Pack,
		description:   args.Description,
		values:        args.Values,
		valuesHash:    args.ValuesHash,
		resources:     args.Resources,
		secrets:       secrets,
		atomic:        args.Atomic,
		wait:          args.Wait,
		timeout:       args.Timeout,
		takeOwnership: args.TakeOwnership,
		maxHistory:    args.MaxHistory,
		dryRun:        args.DryRun,
		recordOnly:    args.RecordOnly,
	}
	if in.pack != nil {
		p := *in.pack
		p.TrustedSigner = ""
		in.pack = &p
	}
	if err := r.srv.releases.runApply(op, in, reply); err != nil {
		return err
	}
	reply.Index = max(1, r.srv.raft.AppliedIndex())
	return nil
}

// checkSecretsHaveVariables rejects secrets for Variables that are not
// resources of the release.
func checkSecretsHaveVariables(secrets map[string]map[string]string, resources []*structs.ReleaseResource, namespace string) error {
	for key := range secrets {
		found := false
		for _, res := range resources {
			if res == nil || res.Type != structs.ReleaseResourceVariable || res.Variable == nil {
				continue
			}
			ns := res.Variable.Namespace
			if ns == "" {
				ns = namespace
			}
			if ns+"/"+res.Variable.Path == key {
				found = true
				break
			}
		}
		if !found {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "secret items for %q, which is not a variable of the pack", key)
		}
	}
	return nil
}

// Rollback applies the resources of an earlier revision as a new revision.
func (r *Release) Rollback(args *structs.ReleaseRollbackRequest, reply *structs.ReleaseApplyResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.Rollback", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "rollback"}, time.Now())

	namespace := args.RequestNamespace()
	if _, err := r.authorize(args, namespace, acl.ObjectCapabilityWrite); err != nil {
		return err
	}
	if err := r.prepareWrite(args.Name, args.DryRun); err != nil {
		return err
	}
	if err := validateTimeout(args.Timeout); err != nil {
		return err
	}

	snap, err := r.srv.State().Snapshot()
	if err != nil {
		return err
	}
	_, rel, err := releaseObject(snap, namespace, args.Name)
	if err != nil {
		return err
	}
	if rel == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "release %q not found", args.Name)
	}
	target := args.Revision
	if target == 0 {
		revs, err := releaseRevisions(snap, namespace, args.Name)
		if err != nil {
			return err
		}
		for i := len(revs) - 1; i >= 0; i-- {
			rev := revs[i]
			if rev.Revision < rel.Status.Revision &&
				(rev.Status.Phase == structs.ReleasePhaseDeployed || rev.Status.Phase == structs.ReleasePhaseSuperseded) {
				target = rev.Revision
				break
			}
		}
		if target == 0 {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "release %q has no earlier revision to roll back to", args.Name)
		}
	}
	rev, _, err := releaseRevision(snap, namespace, args.Name, target)
	if err != nil {
		return err
	}
	if rev == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "revision %d of release %q not found (it may have been pruned)", target, args.Name)
	}

	op, cancel := r.srv.releases.newOp(args, args.AuthToken, namespace, args.Name, args.Timeout)
	defer cancel()
	in := &applyInput{
		action:      structs.ReleaseActionRollback,
		rollbackOf:  target,
		pack:        rev.Spec.Pack,
		description: fmt.Sprintf("Rollback to revision %d", target),
		values:      rev.Spec.Values,
		valuesHash:  rev.Spec.ValuesHash,
		resources:   rev.Resources,
		atomic:      args.Atomic,
		wait:        args.Wait,
		timeout:     args.Timeout,
		dryRun:      args.DryRun,
	}
	if in.pack != nil {
		p := *in.pack
		p.TrustedSigner = ""
		in.pack = &p
	}
	if err := r.srv.releases.runApply(op, in, reply); err != nil {
		return err
	}
	reply.Index = max(1, r.srv.raft.AppliedIndex())
	return nil
}

// Delete uninstalls a release: its pre-delete hooks run, its resources are
// deleted in reverse order (unless they are orphaned), its post-delete hooks
// run, and the release and its revisions are deleted.
func (r *Release) Delete(args *structs.ReleaseDeleteRequest, reply *structs.ReleaseDeleteResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.Delete", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "delete"}, time.Now())

	namespace := args.RequestNamespace()
	if _, err := r.authorize(args, namespace, acl.ObjectCapabilityWrite); err != nil {
		return err
	}
	if err := r.prepareWrite(args.Name, false); err != nil {
		return err
	}
	if err := validateTimeout(args.Timeout); err != nil {
		return err
	}
	switch args.Propagation {
	case "", structs.ReleasePropagationBackground, structs.ReleasePropagationOrphan:
	default:
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid propagation %q, must be %q or %q",
			args.Propagation, structs.ReleasePropagationBackground, structs.ReleasePropagationOrphan)
	}
	orphan := args.Propagation == structs.ReleasePropagationOrphan

	m := r.srv.releases
	release, err := m.acquire(namespace, args.Name)
	if err != nil {
		return err
	}
	defer release()

	snap, err := r.srv.State().Snapshot()
	if err != nil {
		return err
	}
	obj, rel, err := releaseObject(snap, namespace, args.Name)
	if err != nil {
		return err
	}
	if rel == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "release %q not found", args.Name)
	}
	var resources []*structs.ReleaseResource
	if rel.Status.Revision > 0 {
		rev, _, err := releaseRevision(snap, namespace, args.Name, rel.Status.Revision)
		if err != nil {
			return err
		}
		if rev != nil {
			resources = rev.Resources
		}
	}

	op, cancel := m.newOp(args, args.AuthToken, namespace, args.Name, args.Timeout)
	defer cancel()
	rel.Status.Phase = structs.ReleasePhaseUninstalling
	rel.Status.LastOperation = &structs.ReleaseOperation{
		Action: structs.ReleaseActionUninstall, Revision: rel.Status.Revision, User: op.user, StartTime: nowRFC3339(),
	}
	if rel, err = m.writeRelease(rel, obj.ModifyIndex); err != nil {
		return err
	}

	fail := func(cause error) error {
		rel.Status.Phase = structs.ReleasePhaseFailed
		rel.Status.LastOperation.Error = cause.Error()
		rel.Status.LastOperation.EndTime = nowRFC3339()
		if _, err := m.writeLatestRelease(rel); err != nil {
			m.logger.Warn("failed to record failed uninstall", "release", rel.Name, "error", err)
		}
		return fmt.Errorf("uninstalling release %q failed: %w", rel.Name, cause)
	}

	runHooks := !args.NoHooks && !orphan
	if runHooks {
		if err := op.runHooks(resources, structs.ReleaseHookPreDelete); err != nil {
			return fail(err)
		}
	}
	if !orphan {
		refs := make([]*structs.ReleaseResourceRef, 0, len(rel.Status.Resources))
		for _, ref := range rel.Status.Resources {
			if ref != nil {
				refs = append(refs, ref)
			}
		}
		// Hook jobs kept by their deletion policy go with the release.
		for _, res := range resources {
			if res.IsHook() {
				ref := res.Ref()
				ref.Hook = ""
				refs = append(refs, ref)
			}
		}
		sortRefsForDeletion(refs)
		if err := op.deleteResources(refs, false); err != nil {
			return fail(err)
		}
	} else {
		op.orphanObjects(rel)
	}
	if runHooks {
		if err := op.runHooks(resources, structs.ReleaseHookPostDelete); err != nil {
			return fail(err)
		}
	}

	// Delete the revisions, then the release.
	snap, err = r.srv.State().Snapshot()
	if err != nil {
		return err
	}
	revs, err := releaseRevisions(snap, namespace, args.Name)
	if err != nil {
		return err
	}
	for _, rev := range revs {
		if err := m.deleteObject(structs.ReleaseRevisionKind, namespace, structs.ReleaseRevisionObjectName(args.Name, rev.Revision)); err != nil {
			return fail(err)
		}
	}
	if err := m.deleteObject(structs.ReleaseKind, namespace, args.Name); err != nil {
		return err
	}

	reply.Release = rel
	reply.Changes = op.changes
	reply.Index = max(1, r.srv.raft.AppliedIndex())
	return nil
}

// orphanObjects removes the owner references to the release from the
// objects it owns, so that they outlive it.
func (op *releaseOp) orphanObjects(rel *structs.Release) {
	snap, err := op.m.srv.State().Snapshot()
	if err != nil {
		return
	}
	for _, ref := range rel.Status.Resources {
		if ref == nil || ref.Type != structs.ReleaseResourceObject {
			continue
		}
		live, err := op.m.readLive(snap, ref)
		if err != nil || !live.exists() {
			continue
		}
		obj := live.res.Object
		var owners []*structs.ObjectOwnerReference
		for _, o := range obj.Owners {
			if o != nil && !(o.Kind == structs.ReleaseKindRef && o.Namespace == rel.Namespace && o.Name == rel.Name) {
				owners = append(owners, o)
			}
		}
		if len(owners) == len(obj.Owners) {
			continue
		}
		obj.Owners = owners
		if _, err := op.applyResource(live.res); err != nil {
			op.warn("failed to orphan %s: %v", ref, err)
		}
	}
}

// Test runs the test hooks of the deployed revision of a release and
// records the result in its status.
func (r *Release) Test(args *structs.ReleaseTestRequest, reply *structs.ReleaseTestResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.Test", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "test"}, time.Now())

	namespace := args.RequestNamespace()
	if _, err := r.authorize(args, namespace, acl.ObjectCapabilityWrite); err != nil {
		return err
	}
	if err := r.prepareWrite(args.Name, false); err != nil {
		return err
	}
	if err := validateTimeout(args.Timeout); err != nil {
		return err
	}

	m := r.srv.releases
	release, err := m.acquire(namespace, args.Name)
	if err != nil {
		return err
	}
	defer release()

	snap, err := r.srv.State().Snapshot()
	if err != nil {
		return err
	}
	_, rel, err := releaseObject(snap, namespace, args.Name)
	if err != nil {
		return err
	}
	if rel == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "release %q not found", args.Name)
	}
	if rel.Status.Revision == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "release %q has no deployed revision", args.Name)
	}
	rev, _, err := releaseRevision(snap, namespace, args.Name, rel.Status.Revision)
	if err != nil {
		return err
	}
	if rev == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "revision %d of release %q not found", rel.Status.Revision, args.Name)
	}

	op, cancel := m.newOp(args, args.AuthToken, namespace, args.Name, args.Timeout)
	defer cancel()
	result := &structs.ReleaseTestResult{Revision: rev.Revision, StartTime: nowRFC3339()}
	testErr := op.runHooks(rev.Resources, structs.ReleaseHookTest)
	result.EndTime = nowRFC3339()
	result.Hooks = op.hooks
	result.Succeeded = testErr == nil

	rel.Status.LastTest = result
	if _, err := m.writeLatestRelease(rel); err != nil {
		return err
	}
	reply.Result = result
	reply.Index = max(1, r.srv.raft.AppliedIndex())
	return nil
}

// Drift checks a release for drift now and updates its Drifted condition.
func (r *Release) Drift(args *structs.ReleaseDriftRequest, reply *structs.ReleaseDriftResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.Drift", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "drift"}, time.Now())

	namespace := args.RequestNamespace()
	if _, err := r.authorize(args, namespace, acl.ObjectCapabilityRead); err != nil {
		return err
	}
	if err := structs.ValidateReleaseName(args.Name); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	rel, entries, err := r.srv.releases.checkDrift(namespace, args.Name)
	if err != nil {
		return err
	}
	if rel == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "release %q not found", args.Name)
	}
	reply.Drifted = len(entries) > 0
	reply.Revision = rel.Status.Revision
	reply.Resources = entries
	reply.Index = max(1, r.srv.raft.AppliedIndex())
	return nil
}

// List lists the releases of a namespace, or of all namespaces with "*".
func (r *Release) List(args *structs.ReleaseListRequest, reply *structs.ReleaseListResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.List", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "list"}, time.Now())

	namespace := args.RequestNamespace()
	aclObj, err := r.authorize(args, namespace, acl.ObjectCapabilityRead)
	if err != nil {
		return err
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			iter, err := store.CustomObjects(ws, structs.ReleaseObjectGroup, structs.ReleaseKind, namespace, state.SortDefault)
			if err != nil {
				return err
			}
			releases := []*structs.Release{}
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				obj := raw.(*structs.CustomObject)
				if namespace != structs.AllNamespacesSentinel && obj.Namespace != namespace {
					continue
				}
				if !aclObj.AllowObjectOperation(obj.Namespace, structs.ReleaseKindRef, acl.ObjectCapabilityRead) {
					continue
				}
				if args.Prefix != "" && !hasPrefix(obj.Name, args.Prefix) {
					continue
				}
				rel, err := structs.ReleaseFromObject(obj)
				if err != nil {
					return err
				}
				releases = append(releases, rel)
			}
			reply.Releases = releases
			index, err := store.Index(state.TableCustomObjects)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			r.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return r.srv.blockingRPC(&opts)
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// GetRelease returns a release, or nil if it doesn't exist.
func (r *Release) GetRelease(args *structs.ReleaseSpecificRequest, reply *structs.SingleReleaseResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.GetRelease", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "get_release"}, time.Now())

	namespace := args.RequestNamespace()
	if _, err := r.authorize(args, namespace, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			obj, err := store.CustomObjectByName(ws, structs.ReleaseObjectGroup, structs.ReleaseKind, namespace, args.Name)
			if err != nil {
				return err
			}
			reply.Release = nil
			if obj != nil {
				if reply.Release, err = structs.ReleaseFromObject(obj); err != nil {
					return err
				}
				reply.Index = obj.ModifyIndex
			} else {
				index, err := store.Index(state.TableCustomObjects)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			r.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return r.srv.blockingRPC(&opts)
}

// History lists the revisions of a release, newest first, without their
// content.
func (r *Release) History(args *structs.ReleaseSpecificRequest, reply *structs.ReleaseHistoryResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.History", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "history"}, time.Now())

	namespace := args.RequestNamespace()
	if _, err := r.authorize(args, namespace, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			iter, err := store.CustomObjectsByLabel(ws, structs.ReleaseLabel, args.Name, state.SortDefault)
			if err != nil {
				return err
			}
			var revs []*structs.ReleaseRevision
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				obj := raw.(*structs.CustomObject)
				if obj.Group != structs.ReleaseObjectGroup || obj.Kind != structs.ReleaseRevisionKind || obj.Namespace != namespace {
					continue
				}
				rev, err := structs.ReleaseRevisionFromObject(obj)
				if err != nil {
					return err
				}
				revs = append(revs, rev.WithoutContent())
			}
			sortRevisions(revs)
			out := make([]*structs.ReleaseRevision, 0, len(revs))
			for i := len(revs) - 1; i >= 0; i-- {
				out = append(out, revs[i])
			}
			reply.Revisions = out
			index, err := store.Index(state.TableCustomObjects)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			r.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return r.srv.blockingRPC(&opts)
}

// GetRevision returns a revision with its resources. As the resources
// contain the full job specifications, it also requires the read-job
// capability in the namespace of the release.
func (r *Release) GetRevision(args *structs.ReleaseRevisionRequest, reply *structs.SingleReleaseRevisionResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Release.GetRevision", args, args, reply); done {
		return err
	}
	r.srv.MeasureRPCRate("release", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "release", "get_revision"}, time.Now())

	namespace := args.RequestNamespace()
	aclObj, err := r.authorize(args, namespace, acl.ObjectCapabilityRead)
	if err != nil {
		return err
	}
	if !aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityReadJob) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			obj, err := store.CustomObjectByName(ws, structs.ReleaseObjectGroup, structs.ReleaseRevisionKind,
				namespace, structs.ReleaseRevisionObjectName(args.Name, args.Revision))
			if err != nil {
				return err
			}
			reply.Revision = nil
			if obj != nil {
				rev, err := structs.ReleaseRevisionFromObject(obj)
				if err != nil {
					return err
				}
				if rev.Resources, err = structs.DecodeReleaseResources(rev.Spec.Content); err != nil {
					return err
				}
				reply.Revision = rev
				reply.Index = obj.ModifyIndex
			} else {
				index, err := store.Index(state.TableCustomObjects)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			r.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return r.srv.blockingRPC(&opts)
}
