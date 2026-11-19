// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	norcaversion "github.com/bacanoo-io/norca/norca/version"
	"github.com/hashicorp/go-version"
)

// releaseFeatures are the server features a pack can require in the
// requires block of its metadata.
var releaseFeatures = []string{
	"releases",
	"release-hooks",
	"custom-objects",
	"admission-cel",
	"admission-webhooks",
	"admission-release-signatures",
	"quotas",
	"multiregion-deployments",
	"native-service-catalog",
	"backups",
	"secret-store",
	"mtls",
	// Operator framework phase 4: type-bound webhooks, schema migrations
	// and object limits of kinds.
	"object-webhooks",
	"object-migrations",
	"object-limits",
}

// releaseOp is a running operation on a release.
type releaseOp struct {
	m         *releaseManager
	token     string
	user      string
	namespace string
	name      string

	// args is the authenticated request of the operation.
	args structs.RequestWithIdentity

	ctx    context.Context
	wait   bool
	atomic bool

	warnings []string
	changes  []*structs.ReleaseResourceChange
	hooks    []*structs.ReleaseHookResult

	// applied are the changes the operation made: the resources applied
	// with success and the deletions it started. Only these can become
	// resources of the release after a failure.
	applied []*appliedChange

	// attempted is the resource whose apply failed, if one did. An atomic
	// operation restores it too, in case its endpoint wrote before it
	// failed; it is never recorded as a resource of the release.
	attempted *appliedChange

	// waited reports whether the operation waited for the deployments.
	waited bool

	// claim is the claimant of the operation in the claims registry: it
	// claims the resources of the pack while it runs (ATM-4). taken are
	// the resources it takes over from other releases, by key.
	claim resourceClaimant
	taken map[string]string

	// disowned are the resources of the release, by key, that the
	// operation did not delete because they are not the release's
	// (stillReleases, review finding ATM-7): the release no longer lists
	// them. restored are those an atomic restore wrote or deleted.
	disowned map[string]bool
	restored map[string]bool

	// fence is the state the operation put its release in: it writes only
	// while the release is still so (release_leader.go, ATM-12). nil
	// before the operation wrote its release.
	fence *releaseFence
}

// appliedChange is a change made by an operation, with the state of the
// resource before it, so that an atomic operation can restore it.
type appliedChange struct {
	ref    *structs.ReleaseResourceRef
	before *liveResource

	// What the operation left, so that the restore changes only a resource
	// that is still so (review finding ATM-6): deleted for a deletion; for
	// a write, written is what the operation wrote and index the modify
	// index the endpoint answered for it (applyResult.index, 0 if unknown).
	deleted bool
	written *structs.ReleaseResource
	index   uint64
}

// applyInput is an install, upgrade or rollback.
type applyInput struct {
	action      string
	rollbackOf  uint64
	pack        *structs.ReleasePack
	description string
	values      map[string]any
	valuesHash  string
	resources   []*structs.ReleaseResource

	// secrets are secret Variables items by "namespace/path".
	secrets map[string]map[string]string

	atomic        *bool
	wait          bool
	timeout       time.Duration
	takeOwnership bool
	maxHistory    int
	dryRun        bool
	recordOnly    bool

	// variablesSchemas are stored in the revision.
	variablesSchemas []*structs.ReleaseVariablesSchema

	// multiregion describes the multi-region install this region applies
	// its part of, nil for a release of this region only.
	multiregion *structs.ReleaseMultiregion

	// adopted maps the keys of the multiregion jobs that another region
	// registers to that region; global are the global resources that the
	// global region applies. Both are set by runApply.
	adopted map[string]string
	global  []*structs.ReleaseResource

	// serverRendered reports whether the server rendered the pack templates
	// from its verified pack catalog, binding pack.Digest to resources (PK-6).
	serverRendered bool
}

// newOp starts an operation on a release in the current leadership term
// (release_leader.go): it fails on a server that is not the leader. The
// function returned ends it.
func (m *releaseManager) newOp(args structs.RequestWithIdentity, token, namespace, name string, timeout time.Duration) (*releaseOp, context.CancelFunc, error) {
	if timeout <= 0 {
		timeout = structs.DefaultReleaseTimeout
	}
	ctx, cancel, err := m.beginOp(timeout)
	if err != nil {
		return nil, nil, err
	}
	user := admissionUser(args)
	who := user.Name
	if who == "" {
		who = user.AccessorID
	}
	if who == "" {
		who = user.Type
	}
	return &releaseOp{
		m:         m,
		token:     token,
		user:      who,
		namespace: namespace,
		name:      name,
		args:      args,
		ctx:       ctx,
		disowned:  map[string]bool{},
		restored:  map[string]bool{},
	}, cancel, nil
}

func (op *releaseOp) warn(format string, args ...any) {
	op.warnings = append(op.warnings, fmt.Sprintf(format, args...))
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// checkReleaseRequirements checks the requirements of a pack against this
// server.
func checkReleaseRequirements(req *structs.ReleasePackRequires) error {
	if req == nil {
		return nil
	}
	if req.Norca != "" {
		c, err := version.NewConstraint(req.Norca)
		if err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid Norca version requirement %q: %v", req.Norca, err)
		}
		v := version.Must(version.NewVersion(norcaversion.NorcaVersion))
		if !c.Check(v.Core()) {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"the pack requires Norca %s, the servers run %s", req.Norca, norcaversion.NorcaVersion)
		}
	}
	for _, f := range req.Features {
		if !slices.Contains(releaseFeatures, f) {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "the pack requires the feature %q, which the servers don't provide", f)
		}
	}
	return nil
}

// prepareResources validates the resources of a request and returns
// normalized copies in application order: namespaces of namespaced
// resources are filled in, jobs are marked with the release and objects get
// an owner reference to it.
func (m *releaseManager) prepareResources(snap *state.StateSnapshot, op *releaseOp, in []*structs.ReleaseResource, releaseID string) ([]*structs.ReleaseResource, error) {
	if len(in) > structs.MaxReleaseResources {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "a release can have at most %d resources", structs.MaxReleaseResources)
	}

	// Object kinds of the release decide the scope of its objects.
	kinds := map[string]bool{}
	for _, res := range in {
		if res != nil && res.Type == structs.ReleaseResourceObjectKind && res.ObjectKind != nil {
			kinds[res.ObjectKind.KindRef()] = res.ObjectKind.Namespaced
		}
	}

	region := m.srv.Region()
	var out []*structs.ReleaseResource
	seen := map[string]struct{}{}
	var mErr []error
	for i, raw := range in {
		if raw == nil {
			mErr = append(mErr, fmt.Errorf("resource %d is empty", i))
			continue
		}
		if err := raw.Validate(); err != nil {
			mErr = append(mErr, fmt.Errorf("resource %d (%s): %v", i, raw.Source, err))
			continue
		}
		res := raw.Copy()
		switch res.Type {
		case structs.ReleaseResourceVariable:
			if res.Variable.Namespace == "" {
				res.Variable.Namespace = op.namespace
			}
			slices.Sort(res.SecretKeys)
			res.SecretKeys = slices.Compact(res.SecretKeys)
			for _, k := range res.SecretKeys {
				delete(res.Variable.Items, k)
			}
		case structs.ReleaseResourceExternalService:
			if res.ExternalService.Namespace == "" {
				res.ExternalService.Namespace = op.namespace
			}
		case structs.ReleaseResourceObject:
			obj := res.Object
			namespaced, ok := kinds[obj.KindRef()]
			if !ok {
				kind, err := snap.ObjectKindByName(nil, obj.Group, obj.Kind)
				if err != nil {
					return nil, err
				}
				if kind == nil {
					mErr = append(mErr, fmt.Errorf("object %s/%s: object kind %q not found", obj.KindRef(), obj.Name, obj.KindRef()))
					continue
				}
				namespaced = kind.Namespaced
			}
			switch {
			case !namespaced:
				obj.Namespace = ""
			case obj.Namespace == "":
				obj.Namespace = op.namespace
			}
			owners := []*structs.ObjectOwnerReference{}
			for _, o := range obj.Owners {
				if o != nil && o.Kind != structs.ReleaseKindRef {
					owners = append(owners, o)
				}
			}
			obj.Owners = append(owners, &structs.ObjectOwnerReference{
				Kind: structs.ReleaseKindRef, Namespace: op.namespace, Name: op.name, ID: releaseID,
			})
		case structs.ReleaseResourceJob:
			job := res.Job
			if job.Namespace == "" {
				job.Namespace = op.namespace
			}
			if job.Multiregion == nil && (job.Region == "" || job.Region == "global") {
				job.Region = region
			}
			if job.Region != region && job.Multiregion == nil {
				mErr = append(mErr, fmt.Errorf("job %q is in region %q; the resources of a release must be in the region of the release (%q)",
					job.ID, job.Region, region))
				continue
			}
			if job.Meta == nil {
				job.Meta = map[string]string{}
			}
			job.Meta[structs.ReleaseMetaRelease] = op.name
			job.Meta[structs.ReleaseMetaReleaseNamespace] = op.namespace
			// The owner reference of the operator framework, so that the
			// job links to its release. The release deletes its jobs itself
			// (without a tombstone), so the object garbage collector never
			// acts on it, and orphaned jobs stay.
			job.Meta[structs.JobMetaOwner] = structs.ObjectOwnerMetaValue(structs.ReleaseObjectGroup, structs.ReleaseKind, op.namespace, op.name)
			job.Meta[structs.JobMetaOwnerKind] = structs.ReleaseKindRef
		}

		key := res.Ref().Key()
		if _, dup := seen[key]; dup {
			mErr = append(mErr, fmt.Errorf("duplicate resource %s", res.Ref()))
			continue
		}
		seen[key] = struct{}{}
		out = append(out, res)
	}
	if len(mErr) > 0 {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid resources: %v", structs.ReleaseErrors(mErr))
	}
	structs.SortReleaseResources(out)
	return out, nil
}

// releaseOwners maps the keys of the resources owned by releases to the
// owning release ("namespace/name").
func releaseOwners(snap *state.StateSnapshot) (map[string]string, error) {
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.ReleaseKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		rel, err := structs.ReleaseFromObject(raw.(*structs.CustomObject))
		if err != nil {
			continue
		}
		for _, ref := range rel.Status.Resources {
			if ref != nil {
				owners[ref.Key()] = rel.Namespace + "/" + rel.Name
			}
		}
	}
	return owners, nil
}

// releaseClaimKeys returns the keys of the resources an operation claims:
// those of the pack it writes and records, hooks aside.
func releaseClaimKeys(resources []*structs.ReleaseResource) []string {
	keys := make([]string, 0, len(resources))
	for _, res := range resources {
		if !res.IsHook() {
			keys = append(keys, res.Ref().Key())
		}
	}
	return keys
}

// claimConflicts returns the conflict of the resources others claim, nil
// if there is none. Who claims a resource is said to a caller who may write
// it only; a dry run leaves the others out, as its plan reports them as
// denied.
func claimConflicts(aclObj *acl.ACL, resources []*structs.ReleaseResource, others map[string]resourceClaimant, dryRun bool) error {
	var conflicts []string
	for _, res := range resources {
		holder, ok := others[res.Ref().Key()]
		switch {
		case !ok || res.IsHook():
		case allowReleaseResourceApply(aclObj, res):
			conflicts = append(conflicts, fmt.Sprintf("%s is being written by %s", res.Ref(), holder))
		case !dryRun:
			conflicts = append(conflicts, fmt.Sprintf("%s is being written by another operation", res.Ref()))
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return structs.NewReleaseConflictError("resources of the pack are being written by other operations: %s",
		strings.Join(conflicts, "; "))
}

// runApply installs, upgrades or rolls back a release, or plans it.
func (m *releaseManager) runApply(op *releaseOp, in *applyInput, reply *structs.ReleaseApplyResponse) error {
	if !in.dryRun {
		release, err := m.acquire(op.namespace, op.name)
		if err != nil {
			return err
		}
		defer release()
	}

	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	nsObj, err := snap.NamespaceByName(nil, op.namespace)
	if err != nil {
		return err
	}
	if nsObj == nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "namespace %q of the release not found", op.namespace)
	}
	relObj, rel, err := releaseObject(snap, op.namespace, op.name)
	if err != nil {
		return err
	}

	releaseID := uuid.Generate()
	if rel != nil {
		releaseID = rel.ID
	}
	deployed := rel != nil && rel.Status.Revision > 0
	if in.action == "" {
		in.action = structs.ReleaseActionInstall
		if deployed {
			in.action = structs.ReleaseActionUpgrade
		}
	}
	op.atomic = structs.ReleaseAtomicDefault(nsObj)
	if in.atomic != nil {
		op.atomic = *in.atomic
	}
	op.atomic = op.atomic && !in.recordOnly
	op.wait = in.wait && !in.recordOnly
	if in.pack == nil {
		in.pack = &structs.ReleasePack{}
	}

	if err := checkReleaseRequirements(in.pack.Requires); err != nil {
		return err
	}
	resources, err := m.prepareResources(snap, op, in.resources, releaseID)
	if err != nil {
		return err
	}

	// A region of a multi-region install applies its part of the pack. An
	// operation on a release of such an install from this region alone keeps
	// the division of work.
	if in.multiregion == nil && rel != nil && rel.Spec.Multiregion != nil &&
		slices.Contains(rel.Spec.Multiregion.Regions, m.srv.Region()) {
		in.multiregion = rel.Spec.Multiregion.Copy()
	}
	var remoteChanges []*structs.ReleaseResourceChange
	if in.multiregion != nil {
		resources, in.global, in.adopted, remoteChanges = multiregionSplit(in.multiregion, m.srv.Region(), resources)
		op.changes = append(op.changes, remoteChanges...)
	}
	globalKeys := map[string]bool{}
	for _, res := range in.global {
		globalKeys[res.Ref().Key()] = true
	}

	// The caller must be one who may write every resource this region
	// applies, hooks included, before the release, a revision or a
	// resource is written and before anything about the resources is
	// answered (SEC-5). The endpoints of the types check the caller again
	// when the resources are applied; a dry run reports the resources the
	// caller may not write in its plan instead.
	//
	// The one exception is a resource the stored release owns already and
	// that the operation leaves as it is: ownedKeys are the resources in
	// the status of the stored release, never those of the request.
	aclObj, err := op.resolveACL()
	if err != nil {
		return err
	}
	ownedKeys := map[string]bool{}
	if rel != nil {
		for _, ref := range rel.Status.Resources {
			if ref != nil && ref.Hook == "" {
				ownedKeys[ref.Key()] = true
			}
		}
	}
	if !in.dryRun {
		if err := m.authorizeReleaseResources(snap, aclObj, ownedKeys, resources); err != nil {
			return err
		}
		// A job with ACL policies bound to it changes only with a token
		// that holds them (JOG-2), owned or not.
		for _, res := range resources {
			if aclObj == nil || res.Type != structs.ReleaseResourceJob {
				continue
			}
			if _, adopted := in.adopted[res.Ref().Key()]; adopted {
				continue // not written here, see authorizeAdoption
			}
			if err := m.srv.releaseJobBoundPolicies(snap, aclObj, op.args, res.Job); err != nil {
				return err
			}
		}
	}

	warnings, err := m.admitRelease(snap, op, in)
	if err != nil {
		return err
	}
	op.warnings = append(op.warnings, warnings...)

	// Ownership: resources of other releases can only be taken over
	// explicitly, resources of tenants never, and resources another
	// release operation or a pass over a tenant is writing now not at all.
	// The operation claims the resources it writes first and reads their
	// owners from the state as it is after that, not from the snapshot of
	// its start: an owner that recorded its resources since then held their
	// claims until it had (resource_claims.go, review finding ATM-4). A dry
	// run claims nothing; the claims of others are conflicts for it too.
	self := op.namespace + "/" + op.name
	ownerSnap := snap
	if in.dryRun {
		others := m.claims.holders(releaseClaimKeys(resources))
		maps.DeleteFunc(others, func(_ string, holder resourceClaimant) bool {
			return holder.kind == claimRelease && holder.name == self
		})
		if err := claimConflicts(aclObj, resources, others, true); err != nil {
			return err
		}
	} else {
		op.claim = m.claims.claimant(claimRelease, self)
		if others := m.claims.claim(op.claim, releaseClaimKeys(resources)); len(others) > 0 {
			m.claims.release(op.claim)
			return claimConflicts(aclObj, resources, others, false)
		}
		defer m.claims.release(op.claim)
		if ownerSnap, err = m.srv.State().Snapshot(); err != nil {
			return err
		}
	}
	owners, err := releaseOwners(ownerSnap)
	if err != nil {
		return err
	}
	desired := map[string]*structs.ReleaseResource{}
	var conflicts []string
	taken := map[string]string{}
	for _, res := range resources {
		ref := res.Ref()
		desired[ref.Key()] = res
		// Who owns a resource is said to a caller who may write it only: a
		// dry run gets here with others, and its plan reports them as
		// denied.
		if owner, ok := owners[ref.Key()]; ok && owner != self && allowReleaseResourceApply(aclObj, res) {
			conflicts = append(conflicts, fmt.Sprintf("%s belongs to release %q", ref, owner))
			taken[ref.Key()] = owner
		}
	}
	// Resources a tenant created are the tenant's: a release never takes
	// them.
	tenants, err := tenantOwners(ownerSnap)
	if err != nil {
		return err
	}
	var tenantConflicts []string
	for _, res := range resources {
		if tenant, ok := tenants[res.Ref().Key()]; ok && allowReleaseResourceApply(aclObj, res) {
			tenantConflicts = append(tenantConflicts, fmt.Sprintf("%s belongs to the tenant %q", res.Ref(), tenant))
		}
	}
	if len(tenantConflicts) > 0 {
		return structs.NewReleaseConflictError("resources of the pack belong to tenants: %s", strings.Join(tenantConflicts, "; "))
	}
	if len(conflicts) > 0 && !in.takeOwnership {
		return structs.NewReleaseConflictError("resources of the pack belong to other releases (use -take-ownership to take them over): %s",
			strings.Join(conflicts, "; "))
	}
	if err := m.authorizeAdoption(op, in, resources, taken); err != nil {
		return err
	}
	op.taken = taken

	// Resources of the deployed revision that are no longer in the pack are
	// deleted.
	var previous []*structs.ReleaseResourceRef
	if rel != nil {
		previous = rel.Status.Resources
	}
	var removed []*structs.ReleaseResourceRef
	for _, ref := range previous {
		// Global resources the global region now applies are no longer
		// owned here, but they are not deleted.
		if ref == nil || ref.Hook != "" || globalKeys[ref.Key()] {
			continue
		}
		if _, ok := desired[ref.Key()]; !ok {
			removed = append(removed, ref)
		}
	}
	sortRefsForDeletion(removed)

	if in.dryRun {
		changes, err := op.plan(snap, resources, removed, taken, in.adopted, ownedKeys)
		if err != nil {
			return err
		}
		reply.Changes = append(remoteChanges, changes...)
		reply.Release = rel
		reply.Warnings = op.warnings
		if rel != nil {
			reply.Revision = rel.Spec.Revision + 1
		} else {
			reply.Revision = 1
		}
		return nil
	}

	// The next revision number; revisions left behind by an interrupted
	// operation are skipped.
	revNum := uint64(1)
	if rel != nil {
		revNum = rel.Spec.Revision + 1
	}
	revs, err := releaseRevisions(snap, op.namespace, op.name)
	if err != nil {
		return err
	}
	for _, r := range revs {
		if r.Revision >= revNum {
			revNum = r.Revision + 1
		}
	}

	content, digest, err := structs.EncodeReleaseResources(resources)
	if err != nil {
		return err
	}
	schemaSize := 0
	for _, s := range in.variablesSchemas {
		schemaSize += len(s.Schema)
	}
	if limit := structs.MaxReleaseRevisionContentSize - schemaSize; len(content) > limit {
		return structs.NewErrRPCCodedf(http.StatusRequestEntityTooLarge,
			"the rendered resources are too large for a revision (%d bytes compressed, at most %d)",
			len(content), limit)
	}

	start := nowRFC3339()
	maxHistory := in.maxHistory
	if rel == nil {
		rel = &structs.Release{
			ID:        releaseID,
			Namespace: op.namespace,
			Name:      op.name,
			Spec:      &structs.ReleaseSpec{MaxHistory: structs.DefaultReleaseMaxHistory},
			Status:    &structs.ReleaseStatus{Phase: structs.ReleasePhasePending},
		}
	}
	if maxHistory > 0 {
		rel.Spec.MaxHistory = min(maxHistory, structs.MaxReleaseMaxHistory)
	}
	if rel.Spec.MaxHistory <= 0 {
		rel.Spec.MaxHistory = structs.DefaultReleaseMaxHistory
	}
	prevRevision := rel.Status.Revision
	rel.Spec.Pack = in.pack
	rel.Spec.Description = in.description
	rel.Spec.Revision = revNum
	rel.Spec.Atomic = op.atomic
	rel.Spec.Multiregion = in.multiregion
	rel.Status.Phase = structs.ReleasePhaseApplying
	rel.Status.LastOperation = &structs.ReleaseOperation{
		Action: in.action, Revision: revNum, User: op.user, StartTime: start,
	}
	var expected uint64
	if relObj != nil {
		expected = relObj.ModifyIndex
	}
	if op.leadershipEnded() {
		return op.interruptedError()
	}
	if rel, err = m.writeRelease(rel, expected); err != nil {
		return err
	}
	op.fence = newReleaseFence(rel)

	refs := make([]*structs.ReleaseResourceRef, 0, len(resources))
	for _, res := range resources {
		refs = append(refs, res.Ref())
	}
	rev := &structs.ReleaseRevision{
		Namespace: op.namespace,
		Release:   op.name,
		Revision:  revNum,
		Spec: &structs.ReleaseRevisionSpec{
			Release:     op.name,
			Revision:    revNum,
			Action:      in.action,
			RollbackOf:  in.rollbackOf,
			Pack:        in.pack,
			Description: in.description,
			Values:      in.values,
			ValuesHash:  in.valuesHash,
			Resources:   refs,
			Content:     content,
			User:        op.user,
			CreateTime:  start,

			ContentDigest:    digest,
			VariablesSchemas: in.variablesSchemas,
			Multiregion:      in.multiregion,
		},
		Status: &structs.ReleaseRevisionStatus{Phase: structs.ReleasePhasePending},
	}
	revObjIndex := uint64(0)
	if existing, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.ReleaseRevisionKind,
		op.namespace, structs.ReleaseRevisionObjectName(op.name, revNum)); err == nil && existing != nil {
		revObjIndex = existing.ModifyIndex
	}
	if rev, err = op.writeRevision(rev, rel, revObjIndex); err != nil {
		return m.finishFailed(op, rel, nil, prevRevision, previous, err)
	}

	var execErr error
	if !in.recordOnly {
		execErr = op.execute(in, aclObj, ownedKeys, resources, removed)
	} else {
		for _, res := range resources {
			op.changes = append(op.changes, &structs.ReleaseResourceChange{
				Type: res.Type, Namespace: res.Ref().Namespace, Name: res.Ref().Name, Action: structs.ReleaseChangeAdopt,
			})
		}
	}

	if execErr == nil {
		execErr = op.interruption()
	}
	if execErr != nil {
		return m.finishFailed(op, rel, rev, prevRevision, previous, execErr)
	}

	// Record the live state of the resources for drift detection.
	snap, err = m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	var owned []*structs.ReleaseResourceRef
	for _, res := range resources {
		if res.IsHook() {
			continue
		}
		ref := res.Ref()
		live, err := m.readLive(snap, ref)
		if err != nil {
			return err
		}
		if !live.exists() {
			if in.recordOnly {
				op.warn("%s does not exist", ref)
			}
			continue
		}
		ref.ModifyIndex, ref.Version, ref.Hash = live.modifyIndex, live.version, live.refHash
		owned = append(owned, ref)
	}

	now := time.Now()
	rel.Status.Phase = structs.ReleasePhaseDeployed
	rel.Status.Revision = revNum
	rel.Status.Resources = owned
	switch {
	case op.waited || in.recordOnly:
		rel.Status.SetCondition(structs.ReleaseConditionReady, structs.ReleaseConditionTrue, "Deployed", "", now)
	default:
		rel.Status.SetCondition(structs.ReleaseConditionReady, structs.ReleaseConditionUnknown, "NotWaited",
			"the operation did not wait for the deployments", now)
	}
	hookStatus, hookReason := structs.ReleaseConditionTrue, "HooksSucceeded"
	if len(op.hooks) == 0 {
		hookReason = "NoHooks"
	}
	rel.Status.SetCondition(structs.ReleaseConditionHooksSucceeded, hookStatus, hookReason, "", now)
	rel.Status.SetCondition(structs.ReleaseConditionDrifted, structs.ReleaseConditionFalse, "InSync", "", now)
	rel.Status.LastOperation.EndTime = nowRFC3339()
	rel.Status.LastOperation.Warnings = op.warnings

	rev.Status.Phase = structs.ReleasePhaseDeployed
	rev.Status.Hooks = op.hooks
	if _, err := op.writeRevision(rev, rel, rev.ModifyIndex); errors.Is(err, errReleaseInterrupted) {
		return op.interruptedError()
	} else if err != nil {
		op.warn("failed to update revision %d: %v", revNum, err)
	}
	if err := m.supersede(op, prevRevision, revNum); err != nil {
		return op.interruptedError()
	}

	if rel, err = op.writeLatestRelease(rel); err != nil {
		return op.answer(err)
	}
	if len(taken) > 0 {
		m.releaseTakenResources(op, taken)
	}
	m.pruneRevisions(op, rel)

	m.recordEvent(op.namespace, op.name, structs.ObjectEventTypeNormal, "Deployed",
		fmt.Sprintf("%s of revision %d deployed", in.action, revNum), map[string]string{"revision": fmt.Sprint(revNum)})

	reply.Release = rel
	reply.Revision = revNum
	reply.Changes = op.changes
	reply.Warnings = op.warnings
	return nil
}

// finishFailed records a failed operation. An atomic operation restores the
// state before it, so that the previous revision is deployed again. An
// operation that a leader change interrupted records and restores nothing
// (release_leader.go).
func (m *releaseManager) finishFailed(op *releaseOp, rel *structs.Release, rev *structs.ReleaseRevision,
	prevRevision uint64, previous []*structs.ReleaseResourceRef, cause error) error {

	if op.interrupted() {
		return op.interruptedError()
	}
	now := time.Now()
	msg := cause.Error()
	rolledBack := false
	if op.atomic && (len(op.applied) > 0 || op.attempted != nil) {
		rel.Status.Phase = structs.ReleasePhaseRollingBack
		written, err := op.writeLatestRelease(rel)
		switch {
		case errors.Is(err, errReleaseInterrupted):
			return op.interruptedError()
		case err == nil:
			rel = written
		}
		errs := op.restore()
		if op.interrupted() {
			return op.interruptedError()
		}
		if len(errs) > 0 {
			msg = fmt.Sprintf("%s; restoring the previous state failed: %v", msg, structs.ReleaseErrors(errs))
		} else {
			rolledBack = true
		}
	}

	rel.Status.Phase = structs.ReleasePhaseFailed
	if rolledBack || op.atomic {
		rel.Status.Revision = prevRevision
		rel.Status.Resources = m.afterRestore(op, previous)
		reason := "Failed"
		if rolledBack {
			reason = "RolledBack"
		}
		rel.Status.SetCondition(structs.ReleaseConditionReady, structs.ReleaseConditionFalse, reason, msg, now)
	} else {
		// The resources of the failed revision may be partially applied; the
		// release keeps owning everything that exists.
		if rev != nil {
			rel.Status.Revision = rev.Revision
		}
		rel.Status.Resources = m.ownedAfterFailure(op, previous)
		rel.Status.SetCondition(structs.ReleaseConditionReady, structs.ReleaseConditionFalse, "Failed", msg, now)
	}
	for _, h := range op.hooks {
		if h.Status != structs.ReleaseHookSucceeded {
			rel.Status.SetCondition(structs.ReleaseConditionHooksSucceeded, structs.ReleaseConditionFalse,
				"HookFailed", fmt.Sprintf("hook job %q (%s) %s: %s", h.Job, h.Phase, h.Status, h.Message), now)
		}
	}
	if rel.Status.LastOperation != nil {
		rel.Status.LastOperation.EndTime = nowRFC3339()
		rel.Status.LastOperation.Error = msg
		rel.Status.LastOperation.RolledBack = rolledBack
		rel.Status.LastOperation.Warnings = op.warnings
	}

	if rev != nil {
		rev.Status.Phase = structs.ReleasePhaseFailed
		rev.Status.Message = msg
		rev.Status.Hooks = op.hooks
		if _, err := op.writeRevision(rev, rel, rev.ModifyIndex); errors.Is(err, errReleaseInterrupted) {
			return op.interruptedError()
		} else if err != nil {
			m.logger.Warn("failed to update revision", "release", rel.Name, "error", err)
		}
		if !op.atomic {
			if err := m.supersede(op, prevRevision, rev.Revision); err != nil {
				return op.interruptedError()
			}
		}
	}
	if _, err := op.writeLatestRelease(rel); errors.Is(err, errReleaseInterrupted) {
		return op.interruptedError()
	} else if err != nil {
		m.logger.Warn("failed to record failed release operation", "release", rel.Name, "error", err)
	}

	reason := "Failed"
	if rolledBack {
		reason = "RolledBack"
	}
	m.recordEvent(rel.Namespace, rel.Name, structs.ObjectEventTypeWarning, reason, msg, nil)

	if rolledBack {
		return fmt.Errorf("release %q failed and was rolled back to revision %d: %s", rel.Name, prevRevision, cause)
	}
	return fmt.Errorf("release %q failed: %s", rel.Name, msg)
}

// ownedAfterFailure returns the resources a release owns after a failed
// operation that was not rolled back: the previous ones and the ones the
// operation applied (op.applied: applied with success, through the endpoint
// of their type with the token of the caller) that exist. A resource whose
// apply failed, or that was never reached, is not the release's because the
// request named it.
func (m *releaseManager) ownedAfterFailure(op *releaseOp, previous []*structs.ReleaseResourceRef) []*structs.ReleaseResourceRef {
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return previous
	}
	byKey := map[string]*structs.ReleaseResourceRef{}
	var order []string
	add := func(ref *structs.ReleaseResourceRef) {
		if _, ok := byKey[ref.Key()]; !ok {
			order = append(order, ref.Key())
		}
		byKey[ref.Key()] = ref
	}
	for _, ref := range previous {
		add(ref)
	}
	for _, c := range op.applied {
		live, err := m.readLive(snap, c.ref)
		if err != nil {
			continue
		}
		if !live.exists() {
			delete(byKey, c.ref.Key())
			continue
		}
		ref := *c.ref
		ref.ModifyIndex, ref.Version, ref.Hash = live.modifyIndex, live.version, live.refHash
		add(&ref)
	}
	var out []*structs.ReleaseResourceRef
	for _, k := range order {
		if ref, ok := byKey[k]; ok {
			out = append(out, ref)
		}
	}
	return op.withoutDisowned(out)
}

// afterRestore returns the resources a release owns after a failed atomic
// operation: the previous ones, but those it disowned. Of a resource the
// restore wrote or created again the modify index and version recorded are
// those it has now, so that a later removal or uninstall knows it as the
// release's (stillReleases); its hash stays the one recorded before, which
// drift detection compares.
func (m *releaseManager) afterRestore(op *releaseOp, previous []*structs.ReleaseResourceRef) []*structs.ReleaseResourceRef {
	refs := op.withoutDisowned(previous)
	if len(op.restored) == 0 {
		return refs
	}
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return refs
	}
	out := make([]*structs.ReleaseResourceRef, 0, len(refs))
	for _, ref := range refs {
		if ref != nil && op.restored[ref.Key()] {
			if live, err := m.readLive(snap, ref); err == nil && live.exists() {
				fresh := *ref
				fresh.ModifyIndex, fresh.Version = live.modifyIndex, live.version
				ref = &fresh
			}
		}
		out = append(out, ref)
	}
	return out
}

// supersede marks the previously deployed revision as superseded. It fails
// only with errReleaseInterrupted, when a leader change interrupted the
// operation (release_leader.go); other failures are logged.
func (m *releaseManager) supersede(op *releaseOp, prev, current uint64) error {
	if prev == 0 || prev == current {
		return nil
	}
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return nil
	}
	obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.ReleaseRevisionKind,
		op.namespace, structs.ReleaseRevisionObjectName(op.name, prev))
	if err != nil || obj == nil {
		return nil
	}
	rev, err := structs.ReleaseRevisionFromObject(obj)
	if err != nil || rev.Status.Phase == structs.ReleasePhaseSuperseded {
		return nil
	}
	rev.Status.Phase = structs.ReleasePhaseSuperseded
	_, rel, err := releaseObject(snap, op.namespace, op.name)
	if err != nil || rel == nil {
		return nil
	}
	if _, err := op.writeRevision(rev, rel, obj.ModifyIndex); errors.Is(err, errReleaseInterrupted) {
		return err
	} else if err != nil {
		m.logger.Warn("failed to supersede revision", "release", op.name, "revision", prev, "error", err)
	}
	return nil
}

// pruneRevisions deletes the oldest revisions beyond the history limit. The
// deployed revision is always kept.
func (m *releaseManager) pruneRevisions(op *releaseOp, rel *structs.Release) {
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return
	}
	revs, err := releaseRevisions(snap, op.namespace, op.name)
	if err != nil {
		return
	}
	excess := len(revs) - rel.Spec.MaxHistory
	for _, rev := range revs {
		if excess <= 0 {
			break
		}
		if rev.Revision == rel.Status.Revision || rev.Revision == rel.Spec.Revision {
			continue
		}
		if err := op.deleteRevision(rev); err != nil {
			m.logger.Warn("failed to prune revision", "release", op.name, "revision", rev.Revision, "error", err)
			return
		}
		excess--
	}
}

// authorizeAdoption checks the resources an operation takes without applying
// them through the endpoints of their types, which would check the caller's
// ACL (PK-2): with record_only every resource is adopted as it is, and a
// region of a multi-region install adopts the multiregion jobs another region
// registers. The caller needs the capabilities a real apply of each of them
// needs. Taking resources over from other releases changes those releases,
// so it needs the release write capability in their namespaces too.
func (m *releaseManager) authorizeAdoption(op *releaseOp, in *applyInput, resources []*structs.ReleaseResource, taken map[string]string) error {
	if !m.srv.config.ACLEnabled {
		return nil
	}
	if op.args == nil {
		return structs.ErrPermissionDenied
	}
	aclObj, err := m.srv.ResolveACL(op.args)
	if err != nil {
		return err
	}
	for _, res := range resources {
		if res.IsHook() {
			// Hooks are never owned or adopted; record_only skips them.
			continue
		}
		ref := res.Ref()
		if _, adopted := in.adopted[ref.Key()]; !in.recordOnly && !adopted {
			continue
		}
		if !allowReleaseResourceWrite(aclObj, res) {
			return structs.NewErrRPCCodedf(http.StatusForbidden,
				"Permission denied: the token may not write %s, so the release can't adopt it", ref)
		}
	}
	checked := map[string]bool{}
	for _, owner := range taken {
		ns, _, _ := strings.Cut(owner, "/")
		if checked[ns] {
			continue
		}
		checked[ns] = true
		if !aclObj.AllowObjectOperation(ns, structs.ReleaseKindRef, acl.ObjectCapabilityWrite) {
			return structs.NewErrRPCCodedf(http.StatusForbidden,
				"Permission denied: the token may not write releases in namespace %q, so it can't take over resources of release %q", ns, owner)
		}
	}
	return nil
}

// resolveACL returns the ACL of the caller of the operation, nil when ACLs
// are disabled. An operation without an authenticated request is denied.
func (op *releaseOp) resolveACL() (*acl.ACL, error) {
	if !op.m.srv.config.ACLEnabled {
		return nil, nil
	}
	if op.args == nil {
		return nil, structs.ErrPermissionDenied
	}
	return op.m.srv.ResolveACL(op.args)
}

// authorizeReleaseResources refuses an apply that names a resource the
// caller may not write, hook jobs of every phase included: a release holds,
// records and later deletes or runs only what its author could write. A
// resource the release owns and the apply leaves unchanged needs no right
// (ownedUnchanged).
func (m *releaseManager) authorizeReleaseResources(snap *state.StateSnapshot, aclObj *acl.ACL,
	owned map[string]bool, resources []*structs.ReleaseResource) error {

	for _, res := range resources {
		if allowReleaseResourceApply(aclObj, res) {
			continue
		}
		exempt, err := m.ownedUnchanged(snap, owned, res)
		if err != nil {
			return err
		}
		if !exempt {
			return structs.NewErrRPCCodedf(http.StatusForbidden,
				"Permission denied: the token may not write %s, so it can't be a resource of the release", res.Ref())
		}
	}
	return nil
}

// releaseComparesItself reports whether the release decides itself, without
// the endpoint of the type, that a resource of the type is unchanged. Jobs
// and objects are compared by their endpoints and a Variables entry is
// always written.
func releaseComparesItself(resourceType string) bool {
	return resourceType != structs.ReleaseResourceJob && resourceType != structs.ReleaseResourceObject &&
		resourceType != structs.ReleaseResourceVariable
}

// ownedUnchanged reports whether res is a resource the stored release owns
// (owned: the keys of its status) and that is live as res wants it. Such a
// resource is not written by an upgrade or a rollback, and its caller needs
// no right on it: the team of a namespace upgrades the release that an
// operator installed with a namespace, a quota or an ACL policy, as long as
// it leaves those alone.
//
// The live state of a resource that is not owned is never compared here.
// For an owned one the answer says whether the live content is the
// submitted one; a reader of the release has the hash of that content in
// the status of the release and its drift report already.
func (m *releaseManager) ownedUnchanged(snap *state.StateSnapshot, owned map[string]bool, res *structs.ReleaseResource) (bool, error) {
	if res.IsHook() || !releaseComparesItself(res.Type) || !owned[res.Ref().Key()] {
		return false, nil
	}
	live, err := m.readLive(snap, res.Ref())
	if err != nil {
		return false, err
	}
	return !resourceChanged(res, live), nil
}

// allowReleaseResourceApply reports whether the ACL object has what the
// endpoint of the resource's type requires to write it. It is
// allowReleaseResourceWrite without the read of a Variables entry, which
// only adopting and comparing need: an apply writes the entry through its
// endpoint and never compares or returns its items.
func allowReleaseResourceApply(aclObj *acl.ACL, res *structs.ReleaseResource) bool {
	if aclObj != nil && res.Type == structs.ReleaseResourceVariable {
		v := res.Variable
		return aclObj.AllowVariableOperation(v.Namespace, v.Path, acl.VariablesCapabilityWrite, nil)
	}
	return allowReleaseResourceWrite(aclObj, res)
}

// allowReleaseResourceWrite reports whether the ACL object has the
// capabilities the endpoint of the resource's type requires to write it.
func allowReleaseResourceWrite(aclObj *acl.ACL, res *structs.ReleaseResource) bool {
	if aclObj == nil {
		return true
	}
	switch res.Type {
	case structs.ReleaseResourceNamespace, structs.ReleaseResourceACLPolicy:
		return aclObj.IsManagement()
	case structs.ReleaseResourceQuota:
		return aclObj.AllowQuotaWrite()
	case structs.ReleaseResourceVariable:
		// Adopting records the hash of the items, so reading is needed too.
		v := res.Variable
		return aclObj.AllowVariableOperation(v.Namespace, v.Path, acl.VariablesCapabilityWrite, nil) &&
			aclObj.AllowVariableOperation(v.Namespace, v.Path, acl.VariablesCapabilityRead, nil)
	case structs.ReleaseResourceObjectKind:
		return aclObj.AllowObjectKindWrite()
	case structs.ReleaseResourceAdmissionPolicy:
		return aclObj.AllowAdmissionWrite()
	case structs.ReleaseResourceExternalService:
		return aclObj.AllowNsOp(res.ExternalService.Namespace, acl.NamespaceCapabilityRegisterExternalService)
	case structs.ReleaseResourceObject:
		ns := res.Object.Namespace
		if ns == "" {
			ns = acl.ObjectClusterNamespace
		}
		return aclObj.AllowObjectOperation(ns, res.Object.KindRef(), acl.ObjectCapabilityWrite)
	case structs.ReleaseResourceJob:
		return aclObj.AllowNsOp(res.Job.Namespace, acl.NamespaceCapabilitySubmitJob)
	}
	return false
}

// allowReleaseResourceRead reports whether the ACL object has the
// capabilities required to read the live state of a resource.
func allowReleaseResourceRead(aclObj *acl.ACL, res *structs.ReleaseResource) bool {
	if aclObj == nil {
		return true
	}
	switch res.Type {
	case structs.ReleaseResourceNamespace:
		return aclObj.IsManagement() || aclObj.AllowNamespace(res.Namespace.Name)
	case structs.ReleaseResourceACLPolicy:
		return aclObj.IsManagement()
	case structs.ReleaseResourceQuota:
		return aclObj.AllowQuotaRead()
	case structs.ReleaseResourceVariable:
		v := res.Variable
		return aclObj.AllowVariableOperation(v.Namespace, v.Path, acl.VariablesCapabilityRead, nil)
	case structs.ReleaseResourceObjectKind:
		return aclObj.AllowObjectKindRead(res.ObjectKind.KindRef())
	case structs.ReleaseResourceAdmissionPolicy:
		return aclObj.AllowAdmissionRead()
	case structs.ReleaseResourceExternalService:
		return aclObj.AllowNsOp(res.ExternalService.Namespace, acl.NamespaceCapabilityReadJob)
	case structs.ReleaseResourceObject:
		ns := res.Object.Namespace
		if ns == "" {
			ns = acl.ObjectClusterNamespace
		}
		return aclObj.AllowObjectOperation(ns, res.Object.KindRef(), acl.ObjectCapabilityRead)
	case structs.ReleaseResourceJob:
		return aclObj.AllowNsOp(res.Job.Namespace, acl.NamespaceCapabilityReadJob)
	}
	return false
}

// redactReleaseRevision returns a release revision object without its
// encoded content unless aclObj may read every resource in it: the content
// holds the decrypted values of the release's Variables. Other objects are
// returned as they are.
func redactReleaseRevision(aclObj *acl.ACL, obj *structs.CustomObject) *structs.CustomObject {
	if aclObj == nil || obj == nil || obj.KindRef() != structs.ReleaseRevisionKindRef {
		return obj
	}
	if rev, err := structs.ReleaseRevisionFromObject(obj); err == nil {
		if resources, err := structs.DecodeReleaseResources(rev.Spec.Content); err == nil &&
			allowReleaseResourcesRead(aclObj, resources) {
			return obj
		}
	}
	return structs.WithoutRevisionContent(obj)
}

// allowReleaseResourcesRead reports whether aclObj may read every resource.
func allowReleaseResourcesRead(aclObj *acl.ACL, resources []*structs.ReleaseResource) bool {
	for _, res := range resources {
		if !allowReleaseResourceRead(aclObj, res) {
			return false
		}
	}
	return true
}

// releaseTakenResources removes resources taken over by this release from
// the releases that owned them.
func (m *releaseManager) releaseTakenResources(op *releaseOp, taken map[string]string) {
	byOwner := map[string][]string{}
	for key, owner := range taken {
		byOwner[owner] = append(byOwner[owner], key)
	}
	for owner, keys := range byOwner {
		ns, name, _ := strings.Cut(owner, "/")
		for attempt := 0; attempt < 5; attempt++ {
			snap, err := m.srv.State().Snapshot()
			if err != nil {
				return
			}
			obj, rel, err := releaseObject(snap, ns, name)
			if err != nil || rel == nil {
				break
			}
			rel.Status.Resources = slices.DeleteFunc(rel.Status.Resources, func(r *structs.ReleaseResourceRef) bool {
				return r != nil && slices.Contains(keys, r.Key())
			})
			if _, err := m.writeRelease(rel, obj.ModifyIndex); err == nil {
				op.warn("took over %d resources of release %q", len(keys), owner)
				break
			} else if !structs.IsObjectConflictError(err) {
				m.logger.Warn("failed to release taken resources", "release", owner, "error", err)
				break
			}
		}
	}
}

// sortRefsForDeletion sorts references in deletion order: the reverse of
// the application order.
func sortRefsForDeletion(refs []*structs.ReleaseResourceRef) {
	sort.SliceStable(refs, func(i, j int) bool {
		a, b := structs.ReleaseResourceTypeOrder(refs[i].Type), structs.ReleaseResourceTypeOrder(refs[j].Type)
		if a != b {
			return a > b
		}
		if refs[i].Namespace != refs[j].Namespace {
			return refs[i].Namespace > refs[j].Namespace
		}
		return refs[i].Name > refs[j].Name
	})
}

// plan computes the changes an apply would make, without writing.
func (op *releaseOp) plan(snap *state.StateSnapshot, resources []*structs.ReleaseResource,
	removed []*structs.ReleaseResourceRef, taken, adopted map[string]string, owned map[string]bool) ([]*structs.ReleaseResourceChange, error) {

	var aclObj *acl.ACL
	if op.m.srv.config.ACLEnabled {
		if op.args == nil {
			return nil, structs.ErrPermissionDenied
		}
		var err error
		if aclObj, err = op.m.srv.ResolveACL(op.args); err != nil {
			return nil, err
		}
	}

	var changes []*structs.ReleaseResourceChange
	for _, res := range resources {
		ref := res.Ref()
		change := &structs.ReleaseResourceChange{
			Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Hook: ref.Hook, Owner: taken[ref.Key()],
		}
		changes = append(changes, change)
		if res.IsHook() {
			change.Action = structs.ReleaseChangeHook
			continue
		}
		if submitter, ok := adopted[ref.Key()]; ok {
			change.Action, change.Region = structs.ReleaseChangeAdopt, submitter
			continue
		}
		// Never read or compare the live state of a resource the caller may
		// not read or write (PK-3): otherwise a dry run leaks whether a
		// Variable, ACL policy, quota, namespace or object exists and
		// whether its content matches the submitted resource.
		if !allowReleaseResourceWrite(aclObj, res) || !allowReleaseResourceRead(aclObj, res) {
			exempt, err := op.m.ownedUnchanged(snap, owned, res)
			switch {
			case err != nil:
				return nil, err
			case exempt:
				// The apply needs no right for it, see ownedUnchanged.
				change.Action = structs.ReleaseChangeUnchanged
			case res.Type == structs.ReleaseResourceVariable && allowReleaseResourceApply(aclObj, res):
				// Written without being read: the plan does not say
				// whether it exists or differs.
				change.Action = structs.ReleaseChangeUpdate
			default:
				change.Action = structs.ReleaseChangeUpdate
				change.Error = structs.ErrPermissionDenied.Error()
			}
			continue
		}
		live, err := op.m.readLive(snap, ref)
		if err != nil {
			return nil, err
		}

		switch res.Type {
		case structs.ReleaseResourceJob:
			// A job with bound ACL policies the token doesn't hold (JOG-2):
			// the apply would be refused.
			if aclObj != nil {
				if err := op.m.srv.releaseJobBoundPolicies(snap, aclObj, op.args, res.Job); err != nil {
					change.Action = structs.ReleaseChangeUpdate
					if !live.exists() {
						change.Action = structs.ReleaseChangeCreate
					}
					change.Error = err.Error()
					if _, msg, ok := structs.CodeFromRPCCodedErr(err); ok {
						change.Error = msg
					}
					continue
				}
			}
			var reply structs.JobPlanResponse
			job := res.Job.Copy()
			err := op.m.srv.RPC("Job.Plan", &structs.JobPlanRequest{
				Job: job, Diff: true, WriteRequest: op.writeRequest(job.Namespace),
			}, &reply)
			if err != nil {
				change.Error = err.Error()
				change.Action = structs.ReleaseChangeUpdate
				if !live.exists() {
					change.Action = structs.ReleaseChangeCreate
				}
				continue
			}
			change.JobDiff = reply.Diff
			switch {
			case !live.exists():
				change.Action = structs.ReleaseChangeCreate
			case reply.Diff != nil && reply.Diff.Type == structs.DiffTypeNone:
				change.Action = structs.ReleaseChangeUnchanged
			default:
				change.Action = structs.ReleaseChangeUpdate
			}
			if reply.Warnings != "" {
				op.warn("job %q: %s", job.ID, reply.Warnings)
			}
		case structs.ReleaseResourceObject:
			obj := res.Object.Copy()
			ns := obj.Namespace
			if ns == "" {
				ns = op.namespace
			}
			var reply structs.CustomObjectWriteResponse
			err := op.m.srv.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
				Object: obj, DryRun: true, WriteRequest: op.writeRequest(ns),
			}, &reply)
			switch {
			case err != nil && live.exists():
				change.Action, change.Error = structs.ReleaseChangeUpdate, err.Error()
			case err != nil:
				// The kind or the namespace may be created by the release
				// itself; whether one is missing is read from the state,
				// not from the text of the error (review finding JOG-6).
				change.Action = structs.ReleaseChangeCreate
				missing, lookupErr := objectPrerequisiteMissing(snap, obj, ns)
				if lookupErr != nil {
					return nil, lookupErr
				}
				if !missing {
					change.Error = err.Error()
				}
			case !live.exists():
				change.Action = structs.ReleaseChangeCreate
			case reply.Unchanged:
				change.Action = structs.ReleaseChangeUnchanged
			default:
				change.Action = structs.ReleaseChangeUpdate
			}
		default:
			switch {
			case !live.exists():
				change.Action = structs.ReleaseChangeCreate
			case resourceChanged(res, live):
				change.Action = structs.ReleaseChangeUpdate
			default:
				change.Action = structs.ReleaseChangeUnchanged
			}
		}
	}
	for _, ref := range removed {
		changes = append(changes, &structs.ReleaseResourceChange{
			Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: structs.ReleaseChangeDelete,
		})
	}
	return changes, nil
}

// execute runs an install or upgrade: pre hooks, the resources in order, the
// deletions in reverse order, waiting for the deployments, post hooks.
//
// aclObj is the ACL of the caller (nil: ACLs are disabled). Every resource is
// applied through the endpoint of its type, which checks the caller; what is
// decided here without an endpoint is decided with aclObj, or is about a
// resource of owned, the resources the stored release owns.
func (op *releaseOp) execute(in *applyInput, aclObj *acl.ACL, owned map[string]bool, resources []*structs.ReleaseResource, removed []*structs.ReleaseResourceRef) error {
	pre, post := structs.ReleaseHookPreUpgrade, structs.ReleaseHookPostUpgrade
	if in.action == structs.ReleaseActionInstall {
		pre, post = structs.ReleaseHookPreInstall, structs.ReleaseHookPostInstall
	}
	if in.multiregion != nil {
		op.waitReplicated(in.global, in.multiregion.GlobalRegion)
	}
	if err := op.runHooks(resources, pre); err != nil {
		return err
	}

	// Quotas of the release that don't exist yet are attached to the
	// namespaces of the release once they are created.
	quotas := map[string]bool{}
	for _, res := range resources {
		if res.Type == structs.ReleaseResourceQuota {
			quotas[res.Quota.Name] = true
		}
	}
	var deferred []*structs.ReleaseResource
	flushDeferred := func() error {
		for _, res := range deferred {
			ref := res.Ref()
			// The namespace was written without its quota: the restore
			// compares it with this second write now.
			var change *appliedChange
			for _, c := range slices.Backward(op.applied) {
				if c.ref.Key() == ref.Key() && !c.deleted {
					change = c
					break
				}
			}
			if change == nil {
				snap, err := op.m.srv.State().Snapshot()
				if err != nil {
					return err
				}
				before, err := op.m.readLive(snap, ref)
				if err != nil {
					return err
				}
				change = &appliedChange{ref: ref, before: before}
			}
			op.beforeApply(ref)
			if err := op.checkOwner(aclObj, res); err != nil {
				return fmt.Errorf("%s: %w", ref, err)
			}
			result, err := op.applyResource(res)
			if err != nil {
				return fmt.Errorf("%s: %w", ref, err)
			}
			if change.written == nil {
				op.applied = append(op.applied, change)
			}
			change.written, change.index = res, result.index
		}
		deferred = nil
		return nil
	}

	var jobs []*registeredJob
	for _, res := range resources {
		if res.IsHook() {
			continue
		}
		if len(deferred) > 0 && res.Order() > structs.ReleaseResourceTypeOrder(structs.ReleaseResourceQuota) {
			if err := flushDeferred(); err != nil {
				return err
			}
		}
		if err := op.ctx.Err(); err != nil {
			if op.leadershipEnded() {
				return errReleaseInterrupted
			}
			return fmt.Errorf("the operation timed out")
		}

		ref := res.Ref()
		snap, err := op.m.srv.State().Snapshot()
		if err != nil {
			return err
		}
		before, err := op.m.readLive(snap, ref)
		if err != nil {
			return err
		}

		// A multiregion job that another region registers in this region
		// is adopted: the release owns and waits for it, but doesn't
		// register it.
		if submitter, ok := in.adopted[ref.Key()]; ok {
			change := &structs.ReleaseResourceChange{
				Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: structs.ReleaseChangeAdopt, Region: submitter,
			}
			op.changes = append(op.changes, change)
			if !before.exists() {
				change.Error = "not registered in this region"
				return fmt.Errorf("multiregion job %q is registered by the release in region %q, but it does not exist in region %q",
					ref.Name, submitter, op.m.srv.Region())
			}
			jobs = append(jobs, &registeredJob{namespace: ref.Namespace, id: ref.Name})
			continue
		}

		toApply := res
		switch res.Type {
		case structs.ReleaseResourceVariable:
			if toApply, err = op.withSecrets(res, before, in.secrets); err != nil {
				return err
			}
		case structs.ReleaseResourceNamespace:
			if q := res.Namespace.Quota; q != "" && quotas[q] {
				if existing, err := snap.QuotaSpecByName(nil, q); err == nil && existing == nil {
					toApply = res.Copy()
					toApply.Namespace.Quota = ""
					deferred = append(deferred, res)
				}
			}
		}

		action := structs.ReleaseChangeCreate
		if before.exists() {
			action = structs.ReleaseChangeUpdate
			// A resource equal to the live one is not written again and
			// becomes the release's without a call to its endpoint, so the
			// caller's rights are checked here: to write it, as the endpoint
			// would, and to read it, because the answer says whether the
			// live content is the submitted one (SEC-7). Without them the
			// endpoint decides, unless the release owns the resource
			// already (ownedUnchanged).
			if releaseComparesItself(res.Type) &&
				(owned[ref.Key()] || allowReleaseResourceWrite(aclObj, res) && allowReleaseResourceRead(aclObj, res)) &&
				!resourceChanged(toApply, before) {
				op.changes = append(op.changes, &structs.ReleaseResourceChange{
					Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: structs.ReleaseChangeUnchanged,
				})
				continue
			}
		}

		// A Variable is written only in the state it was read in: its secret
		// items may come from it (withSecrets).
		var index *uint64
		if res.Type == structs.ReleaseResourceVariable {
			read := before.modifyIndex // 0: it did not exist
			index = &read
		}
		op.beforeApply(ref)
		if err := op.checkOwner(aclObj, res); err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		result, err := op.applyResourceAt(toApply, index)
		change := &structs.ReleaseResourceChange{Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: action}
		op.changes = append(op.changes, change)
		if err != nil {
			change.Error = err.Error()
			// A resource that changed meanwhile was not written: restoring
			// it would overwrite the change.
			if !errors.Is(err, errReleaseResourceChanged) {
				op.attempted = &appliedChange{ref: ref, before: before, written: toApply}
			}
			return fmt.Errorf("%s: %w", ref, err)
		}
		// Applied: only now is the resource one the release may keep as its
		// own after a failure (SEC-5). One whose endpoint refused it is not.
		op.applied = append(op.applied, &appliedChange{ref: ref, before: before, written: toApply, index: result.index})
		if result.warnings != "" {
			op.warn("%s: %s", ref, result.warnings)
		}
		if res.Type == structs.ReleaseResourceJob {
			jobs = append(jobs, &registeredJob{namespace: ref.Namespace, id: ref.Name, evalID: result.evalID})
		}
	}
	if err := flushDeferred(); err != nil {
		return err
	}

	if err := op.deleteResources(removed, true); err != nil {
		return err
	}

	if op.wait {
		if err := op.waitJobs(jobs); err != nil {
			return err
		}
		op.waited = true
	}

	return op.runHooks(resources, post)
}

// checkOwner checks, right before the operation writes a resource of its
// pack, that the resource is still the operation's to write (ATM-4): it
// claims the resource, and in the state as it is now no tenant owns it and
// no other release does, unless the operation takes it over from that
// release. The claim makes the state check redundant on one leader; it
// counts after a change of leader, whose claims do not include this
// operation's.
func (op *releaseOp) checkOwner(aclObj *acl.ACL, res *structs.ReleaseResource) error {
	ref := res.Ref()
	key := ref.Key()
	if !op.m.claims.holds(op.claim, key) {
		return structs.NewReleaseConflictError("the operation does not claim %s", ref)
	}
	snap, err := op.m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	tenants, err := tenantOwners(snap)
	if err != nil {
		return err
	}
	owner, ok := "", false
	if tenant, isTenant := tenants[key]; isTenant {
		owner, ok = fmt.Sprintf("the tenant %q", tenant), true
	} else {
		releases, err := releaseOwners(snap)
		if err != nil {
			return err
		}
		if rel, isRelease := releases[key]; isRelease && rel != op.namespace+"/"+op.name && op.taken[key] == "" {
			owner, ok = fmt.Sprintf("release %q", rel), true
		}
	}
	switch {
	case !ok:
		return nil
	case allowReleaseResourceApply(aclObj, res):
		return structs.NewReleaseConflictError("%s belongs to %s now", ref, owner)
	default:
		return structs.NewReleaseConflictError("%s has another owner now", ref)
	}
}

// beforeApply runs the test hook releaseBeforeApply, if set.
func (op *releaseOp) beforeApply(ref *structs.ReleaseResourceRef) {
	if cfg := op.m.srv.config; cfg != nil && cfg.releaseBeforeApply != nil {
		cfg.releaseBeforeApply(op.namespace+"/"+op.name, ref)
	}
}

// beforeDelete runs the test hook releaseBeforeDelete, if set.
func (op *releaseOp) beforeDelete(ref *structs.ReleaseResourceRef) {
	if cfg := op.m.srv.config; cfg != nil && cfg.releaseBeforeDelete != nil {
		cfg.releaseBeforeDelete(op.namespace+"/"+op.name, ref)
	}
}

// beforeRevision runs the test hook releaseBeforeRevision, if set.
func (op *releaseOp) beforeRevision(rev *structs.ReleaseRevision, deleting bool) {
	if cfg := op.m.srv.config; cfg != nil && cfg.releaseBeforeRevision != nil {
		cfg.releaseBeforeRevision(op.namespace+"/"+op.name, rev, deleting)
	}
}

// deleteResources deletes resources in the given (deletion) order. Quotas
// are detached from namespaces that are deleted with them first. With
// record, the deletions are recorded for an atomic restore.
func (op *releaseOp) deleteResources(refs []*structs.ReleaseResourceRef, record bool) error {
	quotas := map[string]bool{}
	for _, ref := range refs {
		if ref.Type == structs.ReleaseResourceQuota {
			quotas[ref.Name] = true
		}
	}
	var errs []error
	for _, ref := range refs {
		if op.disowned[ref.Key()] {
			continue
		}
		snap, before, why, err := op.stillReleases(ref)
		if err != nil {
			return err
		}
		if !before.exists() {
			continue
		}
		if why != "" {
			op.disown(ref, why)
			continue
		}
		if ref.Type == structs.ReleaseResourceQuota {
			// Detach the quota from the namespaces deleted with it, if
			// they are still the release's.
			for _, nsRef := range refs {
				if nsRef.Type != structs.ReleaseResourceNamespace || op.disowned[nsRef.Key()] {
					continue
				}
				_, live, why, err := op.stillReleases(nsRef)
				if err != nil {
					return err
				}
				if why != "" && live.exists() {
					op.disown(nsRef, why)
					continue
				}
				ns, err := snap.NamespaceByName(nil, nsRef.Name)
				if err != nil || ns == nil || ns.Quota != ref.Name {
					continue
				}
				detached := ns.Copy()
				detached.Quota = ""
				if _, err := op.applyResource(&structs.ReleaseResource{Type: structs.ReleaseResourceNamespace, Namespace: detached}); err != nil {
					errs = append(errs, fmt.Errorf("%s: detaching quota: %w", nsRef, err))
				}
			}
		}
		change := &structs.ReleaseResourceChange{Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: structs.ReleaseChangeDelete}
		op.changes = append(op.changes, change)
		// Deleted as read, where the endpoint can check it; otherwise the
		// deletion RPC itself is the window left after the read.
		var index *uint64
		if releaseDeletesChecked(ref.Type) {
			index = &before.modifyIndex
		}
		op.beforeDelete(ref)
		err = op.deleteResourceAt(ref, index)
		if errors.Is(err, errReleaseInterrupted) {
			return err
		}
		if errors.Is(err, errReleaseResourceChanged) {
			// Changed since the read: left as it is.
			change.Error = "not deleted: " + err.Error()
			op.warn("%s was not deleted: it changed while the release deleted it", ref)
			continue
		}
		if record {
			op.applied = append(op.applied, &appliedChange{ref: ref, before: before, deleted: true})
		}
		if err != nil {
			change.Error = err.Error()
			if record {
				return fmt.Errorf("deleting %s: %w", ref, err)
			}
			errs = append(errs, fmt.Errorf("deleting %s: %w", ref, err))
		}
	}
	return structs.ReleaseErrors(errs)
}

// stillReleases reads a resource the release recorded (ref, from its
// status) right before the release deletes it, and says why it is not the
// release's to delete, "" when it is (review finding ATM-7). A resource
// deleted and created again since the release recorded it (its create index
// is newer than the modify index recorded) is somebody else's, even with
// the release's name; one only changed since (drift) is still the
// release's. Nor is one the release's that a tenant or another release owns
// in the state as it is now, or that another release operation or a pass
// over a tenant claims (resource_claims.go): the operation claims the
// resource first, then reads. A resource recorded without a modify index
// (the hook jobs of an uninstall, which the caller knows by the marks of the
// release) is checked for its owners only.
func (op *releaseOp) stillReleases(ref *structs.ReleaseResourceRef) (*state.StateSnapshot, *liveResource, string, error) {
	key := ref.Key()
	var held string
	if op.claim.id != 0 {
		if others := op.m.claims.claim(op.claim, []string{key}); len(others) > 0 {
			held = fmt.Sprintf("it is being written by %s", others[key])
		}
	}
	snap, err := op.m.srv.State().Snapshot()
	if err != nil {
		return nil, nil, "", err
	}
	live, err := op.m.readLive(snap, ref)
	if err != nil || !live.exists() || held != "" {
		return snap, live, held, err
	}
	tenants, err := tenantOwners(snap)
	if err != nil {
		return nil, nil, "", err
	}
	if tenant, ok := tenants[key]; ok {
		return snap, live, fmt.Sprintf("it belongs to the tenant %q", tenant), nil
	}
	self := op.namespace + "/" + op.name
	other, err := otherReleaseOwner(snap, key, self)
	if err != nil {
		return nil, nil, "", err
	}
	if other != "" {
		return snap, live, fmt.Sprintf("it belongs to release %q", other), nil
	}
	if ref.ModifyIndex != 0 && live.createIndex > ref.ModifyIndex {
		return snap, live, "it was deleted and created again since the release recorded it", nil
	}
	return snap, live, "", nil
}

// otherReleaseOwner returns a release other than self ("namespace/name")
// that lists the resource of the key in its status, "" if none does.
func otherReleaseOwner(snap *state.StateSnapshot, key, self string) (string, error) {
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.ReleaseKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return "", err
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		rel, err := structs.ReleaseFromObject(raw.(*structs.CustomObject))
		if err != nil || rel.Namespace+"/"+rel.Name == self {
			continue
		}
		for _, ref := range rel.Status.Resources {
			if ref != nil && ref.Key() == key {
				return rel.Namespace + "/" + rel.Name, nil
			}
		}
	}
	return "", nil
}

// disown records that the operation leaves a resource of the release that is
// not the release's to delete (stillReleases): it is not deleted, the
// operation warns, and the release no longer lists it.
func (op *releaseOp) disown(ref *structs.ReleaseResourceRef, why string) {
	op.disowned[ref.Key()] = true
	op.warn("%s was not deleted: %s; the release no longer lists it", ref, why)
	op.changes = append(op.changes, &structs.ReleaseResourceChange{
		Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: structs.ReleaseChangeDelete,
		Error: "not deleted: " + why,
	})
}

// withoutDisowned returns refs without the resources the operation
// disowned.
func (op *releaseOp) withoutDisowned(refs []*structs.ReleaseResourceRef) []*structs.ReleaseResourceRef {
	if len(op.disowned) == 0 {
		return refs
	}
	out := make([]*structs.ReleaseResourceRef, 0, len(refs))
	for _, ref := range refs {
		if ref != nil && !op.disowned[ref.Key()] {
			out = append(out, ref)
		}
	}
	return out
}

// withSecrets returns the Variable to write: the items of the pack plus the
// secret items, from the request or else from the stored Variable.
func (op *releaseOp) withSecrets(res *structs.ReleaseResource, before *liveResource, secrets map[string]map[string]string) (*structs.ReleaseResource, error) {
	v := res.Variable
	provided := secrets[v.Namespace+"/"+v.Path]
	if len(res.SecretKeys) == 0 && len(provided) == 0 {
		return res, nil
	}
	out := res.Copy()
	if out.Variable.Items == nil {
		out.Variable.Items = map[string]string{}
	}
	keys := slices.Clone(res.SecretKeys)
	for k := range provided {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if val, ok := provided[k]; ok {
			out.Variable.Items[k] = val
			continue
		}
		if before.exists() {
			if val, ok := before.res.Variable.Items[k]; ok {
				out.Variable.Items[k] = val
				continue
			}
		}
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
			"the secret item %q of the variable %q (namespace %q) has no value; supply it with -var-secret", k, v.Path, v.Namespace)
	}
	return out, nil
}

// restore undoes the changes of a failed atomic operation in reverse order.
func (op *releaseOp) restore() []error {
	changes := op.applied
	if op.attempted != nil {
		// The failed apply was the last change.
		changes = append(slices.Clip(changes), op.attempted)
	}
	var errs []error
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if err := op.restoreChange(c, c == op.attempted); err != nil {
			errs = append(errs, fmt.Errorf("restoring %s: %w", c.ref, err))
			if errors.Is(err, errReleaseInterrupted) {
				break
			}
		}
	}
	return errs
}

// restoreChange puts a resource back as it was before the operation, but
// only if it is still as the operation left it (review finding ATM-6): what
// somebody else changed, deleted or created since is theirs, and the
// operation leaves it and warns. The resource is read again right before,
// and written back with check-and-set against what was read where its
// endpoint has one (releaseWritesChecked, releaseDeletesChecked); a deletion
// of the other types is unconditional, so the deletion RPC itself is the
// window left. attempted is the change whose apply failed: whether its
// endpoint wrote anything is not known.
func (op *releaseOp) restoreChange(c *appliedChange, attempted bool) error {
	snap, err := op.m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	live, err := op.m.readLive(snap, c.ref)
	if err != nil {
		return err
	}
	switch {
	case !live.exists() && !c.before.exists(),
		live.exists() && c.before.exists() && live.modifyIndex == c.before.modifyIndex:
		// As it was before: nothing to restore.
		return nil
	case !op.leftByOperation(c, live, attempted):
		what := "it changed after the operation wrote it"
		if c.deleted {
			what = "it was created again after the operation deleted it"
		}
		op.warn("%s was not restored: %s, and is left as it is", c.ref, what)
		return nil
	}

	op.beforeRestore(c.ref)
	read := live.modifyIndex // 0: it does not exist
	if c.before.exists() {
		var index *uint64
		if releaseWritesChecked(c.ref.Type) {
			index = &read
		}
		_, err = op.applyResourceAt(c.before.res, index)
	} else {
		var index *uint64
		if releaseDeletesChecked(c.ref.Type) {
			index = &read
		}
		err = op.deleteResourceAt(c.ref, index)
	}
	if errors.Is(err, errReleaseResourceChanged) {
		op.warn("%s was not restored: it changed while the operation restored it, and is left as it is", c.ref)
		return nil
	}
	if err == nil {
		op.restored[c.ref.Key()] = true
	}
	return err
}

// leftByOperation reports whether the live resource is still as the change
// left it: deleted, or written by the operation and not changed since. A
// write is compared by the modify index its endpoint answered, or else by
// its content. Jobs and objects can't be compared by their content
// (resourceChanged): of the failed apply of one (attempted, no index) the
// marks of the release tell, which only the release writes.
func (op *releaseOp) leftByOperation(c *appliedChange, live *liveResource, attempted bool) bool {
	switch {
	case c.deleted:
		return !live.exists()
	case !live.exists() || c.written == nil:
		return false
	case !attempted && c.index != 0:
		return live.modifyIndex == c.index
	case c.ref.Type == structs.ReleaseResourceJob:
		return attempted && jobOfRelease(live.res.Job, op.namespace, op.name)
	case c.ref.Type == structs.ReleaseResourceObject:
		return attempted && slices.ContainsFunc(live.res.Object.Owners, func(o *structs.ObjectOwnerReference) bool {
			return o != nil && o.Kind == structs.ReleaseKindRef && o.Namespace == op.namespace && o.Name == op.name
		})
	}
	return !resourceChanged(c.written, live)
}

// beforeRestore runs the test hook releaseBeforeRestore, if set.
func (op *releaseOp) beforeRestore(ref *structs.ReleaseResourceRef) {
	if cfg := op.m.srv.config; cfg != nil && cfg.releaseBeforeRestore != nil {
		cfg.releaseBeforeRestore(op.namespace+"/"+op.name, ref)
	}
}

// secretsByPath indexes the secrets of a request by "namespace/path".
func secretsByPath(secrets []*structs.ReleaseVariableSecret, defaultNS string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, s := range secrets {
		if s == nil {
			continue
		}
		if s.Path == "" {
			return nil, errors.New("secret without a variable path")
		}
		ns := s.Namespace
		if ns == "" {
			ns = defaultNS
		}
		key := ns + "/" + s.Path
		if out[key] == nil {
			out[key] = map[string]string{}
		}
		maps.Copy(out[key], s.Items)
	}
	return out, nil
}
