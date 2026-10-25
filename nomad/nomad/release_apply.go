// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

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

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	norcaversion "github.com/hashicorp/nomad/version"
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
}

// releaseOp is a running operation on a release.
type releaseOp struct {
	m         *releaseManager
	token     string
	user      string
	namespace string
	name      string

	ctx    context.Context
	wait   bool
	atomic bool

	warnings []string
	changes  []*structs.ReleaseResourceChange
	applied  []*appliedChange
	hooks    []*structs.ReleaseHookResult

	// waited reports whether the operation waited for the deployments.
	waited bool
}

// appliedChange is a change made by an operation, with the state of the
// resource before it, so that an atomic operation can restore it.
type appliedChange struct {
	ref    *structs.ReleaseResourceRef
	before *liveResource
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
}

func (m *releaseManager) newOp(args structs.RequestWithIdentity, token, namespace, name string, timeout time.Duration) (*releaseOp, context.CancelFunc) {
	if timeout <= 0 {
		timeout = structs.DefaultReleaseTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
		ctx:       ctx,
	}, cancel
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
	warnings, err := m.admitRelease(snap, op, in)
	if err != nil {
		return err
	}
	op.warnings = append(op.warnings, warnings...)

	// Ownership: resources of other releases can only be taken over
	// explicitly.
	owners, err := releaseOwners(snap)
	if err != nil {
		return err
	}
	self := op.namespace + "/" + op.name
	desired := map[string]*structs.ReleaseResource{}
	var conflicts []string
	taken := map[string]string{}
	for _, res := range resources {
		ref := res.Ref()
		desired[ref.Key()] = res
		if owner, ok := owners[ref.Key()]; ok && owner != self {
			conflicts = append(conflicts, fmt.Sprintf("%s belongs to release %q", ref, owner))
			taken[ref.Key()] = owner
		}
	}
	if len(conflicts) > 0 && !in.takeOwnership {
		return structs.NewReleaseConflictError("resources of the pack belong to other releases (use -take-ownership to take them over): %s",
			strings.Join(conflicts, "; "))
	}

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
		changes, err := op.plan(snap, resources, removed, taken, in.adopted)
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
	if rel, err = m.writeRelease(rel, expected); err != nil {
		return err
	}

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
	if rev, err = m.writeRevision(rev, rel, revObjIndex); err != nil {
		return m.finishFailed(op, rel, nil, prevRevision, previous, err)
	}

	var execErr error
	if !in.recordOnly {
		execErr = op.execute(in, resources, removed)
	} else {
		for _, res := range resources {
			op.changes = append(op.changes, &structs.ReleaseResourceChange{
				Type: res.Type, Namespace: res.Ref().Namespace, Name: res.Ref().Name, Action: structs.ReleaseChangeAdopt,
			})
		}
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
		ref.ModifyIndex, ref.Version, ref.Hash = live.modifyIndex, live.version, live.hash
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
	if _, err := m.writeRevision(rev, rel, rev.ModifyIndex); err != nil {
		op.warn("failed to update revision %d: %v", revNum, err)
	}
	m.supersede(op, prevRevision, revNum)

	if rel, err = m.writeLatestRelease(rel); err != nil {
		return err
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

// writeLatestRelease writes the release over the latest stored version: the
// status of a release changes only while the operation holds its lock, but
// the drift checker may have updated it.
func (m *releaseManager) writeLatestRelease(rel *structs.Release) (*structs.Release, error) {
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return nil, err
	}
	obj, _, err := releaseObject(snap, rel.Namespace, rel.Name)
	if err != nil {
		return nil, err
	}
	var expected uint64
	if obj != nil {
		expected = obj.ModifyIndex
	}
	return m.writeRelease(rel, expected)
}

// finishFailed records a failed operation. An atomic operation restores the
// state before it, so that the previous revision is deployed again.
func (m *releaseManager) finishFailed(op *releaseOp, rel *structs.Release, rev *structs.ReleaseRevision,
	prevRevision uint64, previous []*structs.ReleaseResourceRef, cause error) error {

	now := time.Now()
	msg := cause.Error()
	rolledBack := false
	if op.atomic && len(op.applied) > 0 {
		rel.Status.Phase = structs.ReleasePhaseRollingBack
		if written, err := m.writeLatestRelease(rel); err == nil {
			rel = written
		}
		if errs := op.restore(); len(errs) > 0 {
			msg = fmt.Sprintf("%s; restoring the previous state failed: %v", msg, structs.ReleaseErrors(errs))
		} else {
			rolledBack = true
		}
	}

	rel.Status.Phase = structs.ReleasePhaseFailed
	if rolledBack || op.atomic {
		rel.Status.Revision = prevRevision
		rel.Status.Resources = previous
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
		if _, err := m.writeRevision(rev, rel, rev.ModifyIndex); err != nil {
			m.logger.Warn("failed to update revision", "release", rel.Name, "error", err)
		}
		if !op.atomic {
			m.supersede(op, prevRevision, rev.Revision)
		}
	}
	if _, err := m.writeLatestRelease(rel); err != nil {
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
// operation that was not rolled back: the previous ones and the applied
// ones that exist.
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
		ref.ModifyIndex, ref.Version, ref.Hash = live.modifyIndex, live.version, live.hash
		add(&ref)
	}
	var out []*structs.ReleaseResourceRef
	for _, k := range order {
		if ref, ok := byKey[k]; ok {
			out = append(out, ref)
		}
	}
	return out
}

// supersede marks the previously deployed revision as superseded.
func (m *releaseManager) supersede(op *releaseOp, prev, current uint64) {
	if prev == 0 || prev == current {
		return
	}
	snap, err := m.srv.State().Snapshot()
	if err != nil {
		return
	}
	obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.ReleaseRevisionKind,
		op.namespace, structs.ReleaseRevisionObjectName(op.name, prev))
	if err != nil || obj == nil {
		return
	}
	rev, err := structs.ReleaseRevisionFromObject(obj)
	if err != nil || rev.Status.Phase == structs.ReleasePhaseSuperseded {
		return
	}
	rev.Status.Phase = structs.ReleasePhaseSuperseded
	_, rel, err := releaseObject(snap, op.namespace, op.name)
	if err != nil || rel == nil {
		return
	}
	if _, err := m.writeRevision(rev, rel, obj.ModifyIndex); err != nil {
		m.logger.Warn("failed to supersede revision", "release", op.name, "revision", prev, "error", err)
	}
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
		if err := m.deleteObject(structs.ReleaseRevisionKind, op.namespace,
			structs.ReleaseRevisionObjectName(op.name, rev.Revision)); err != nil {
			m.logger.Warn("failed to prune revision", "release", op.name, "revision", rev.Revision, "error", err)
			return
		}
		excess--
	}
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
	removed []*structs.ReleaseResourceRef, taken, adopted map[string]string) ([]*structs.ReleaseResourceChange, error) {

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
		live, err := op.m.readLive(snap, ref)
		if err != nil {
			return nil, err
		}

		switch res.Type {
		case structs.ReleaseResourceJob:
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
				// The kind may be created by the release itself.
				change.Action = structs.ReleaseChangeCreate
				if !isNotFoundError(err) {
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
func (op *releaseOp) execute(in *applyInput, resources []*structs.ReleaseResource, removed []*structs.ReleaseResourceRef) error {
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
			if _, err := op.applyResource(res); err != nil {
				return fmt.Errorf("%s: %w", res.Ref(), err)
			}
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
			if res.Type != structs.ReleaseResourceJob && res.Type != structs.ReleaseResourceObject &&
				res.Type != structs.ReleaseResourceVariable && !resourceChanged(toApply, before) {
				op.changes = append(op.changes, &structs.ReleaseResourceChange{
					Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: structs.ReleaseChangeUnchanged,
				})
				continue
			}
		}

		op.applied = append(op.applied, &appliedChange{ref: ref, before: before})
		result, err := op.applyResource(toApply)
		change := &structs.ReleaseResourceChange{Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: action}
		op.changes = append(op.changes, change)
		if err != nil {
			change.Error = err.Error()
			return fmt.Errorf("%s: %w", ref, err)
		}
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
		snap, err := op.m.srv.State().Snapshot()
		if err != nil {
			return err
		}
		before, err := op.m.readLive(snap, ref)
		if err != nil {
			return err
		}
		if !before.exists() {
			continue
		}
		if ref.Type == structs.ReleaseResourceQuota {
			// Detach the quota from the namespaces deleted with it.
			for _, nsRef := range refs {
				if nsRef.Type != structs.ReleaseResourceNamespace {
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
		if record {
			op.applied = append(op.applied, &appliedChange{ref: ref, before: before})
		}
		change := &structs.ReleaseResourceChange{Type: ref.Type, Namespace: ref.Namespace, Name: ref.Name, Action: structs.ReleaseChangeDelete}
		op.changes = append(op.changes, change)
		if err := op.deleteResource(ref); err != nil {
			change.Error = err.Error()
			if record {
				return fmt.Errorf("deleting %s: %w", ref, err)
			}
			errs = append(errs, fmt.Errorf("deleting %s: %w", ref, err))
		}
	}
	return structs.ReleaseErrors(errs)
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
	var errs []error
	for i := len(op.applied) - 1; i >= 0; i-- {
		c := op.applied[i]
		var err error
		if c.before.exists() {
			_, err = op.applyResource(c.before.res)
		} else {
			err = op.deleteResource(c.ref)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("restoring %s: %w", c.ref, err))
		}
	}
	return errs
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
