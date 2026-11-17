// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
)

// The operator of tenants keeps the resources of every Tenant object: its
// namespace (with the namespace meta that says what it may ask certificates
// for), its quota, an ACL policy and an ACL role per role, and the network
// policy that isolates the namespace. It writes them through the endpoints
// of their types with the rights of the servers, and lists what it created
// in the status of the tenant: these resources have no owner field.
//
// The status is the only record of what is the tenant's, so a pass writes
// the resources it is about to create or adopt into the status first, and
// writes none of them when that write fails: a status write lost later (the
// object changed during the pass, the leader changed) can't make the tenant
// forget a resource it created. A listed resource that is not known to
// exist is marked pending.
//
// A resource the tenant took over (spec.adopt) is on loan: the status marks
// it adopted and keeps what the tenant found (tenant_restore.go). While the
// tenant lives it is kept like one the tenant created. It is never deleted
// by the tenant: when it leaves the spec or the tenant is deleted, what the
// tenant found is written back and the resource is released.
//
// A resource of a tenant's name that the tenant did not create is left
// alone (condition Conflict), unless spec.adopt names its type; a resource
// of a release or of another tenant is never taken. A change made to a
// resource of the tenant through its own API is set back and reported as an
// event. When the tenant is deleted, the operator waits until its namespace
// has nothing the deletion of a namespace refuses (jobs, volumes, variables,
// objects; condition NamespaceNotEmpty), then deletes what the tenant
// created: the network policy and the namespace first, the roles and the
// policies when the namespace is gone, its quota last.

var (
	// tenantDriftInterval is how often the operator compares the resources
	// of the tenants with what they should be when nothing it watches
	// changed.
	tenantDriftInterval = 5 * time.Minute

	// tenantDeleteRetry is how long a deletion that waits for the namespace
	// to empty waits at most before it looks again.
	tenantDeleteRetry = 30 * time.Second
)

// tenantOperator is the built-in operator of tenants.
type tenantOperator struct {
	srv    *Server
	logger hclog.Logger
}

func newTenantOperator(srv *Server, logger hclog.Logger) *tenantOperator {
	return &tenantOperator{srv: srv, logger: logger.Named("tenants")}
}

func (o *tenantOperator) kinds() []*structs.ObjectKind { return builtinTenantKinds() }

// tenantPass is what one pass of the operator read.
type tenantPass struct {
	now   time.Time
	store *state.StateStore
	ws    memdb.WatchSet

	// releases and tenants map the keys of the resources of releases and
	// tenants to their owner.
	releases map[string]string
	tenants  map[string]string

	wish time.Time
}

func (p *tenantPass) want(t time.Time) {
	if !t.IsZero() && (p.wish.IsZero() || t.Before(p.wish)) {
		p.wish = t
	}
}

func (o *tenantOperator) reconcile(ws memdb.WatchSet) (time.Time, error) {
	store := o.srv.State()
	tenants, err := listObjects(store, ws, structs.TenantKind, "")
	if err != nil {
		return time.Time{}, err
	}
	if len(tenants) == 0 {
		return time.Time{}, nil
	}
	snap, err := store.Snapshot()
	if err != nil {
		return time.Time{}, err
	}
	p := &tenantPass{now: time.Now().UTC(), store: store, ws: ws}
	if p.releases, err = releaseOwners(snap); err != nil {
		return time.Time{}, err
	}
	if p.tenants, err = tenantOwners(snap); err != nil {
		return time.Time{}, err
	}
	for _, t := range tenants {
		if t.Deleting() {
			o.finishDeletion(p, t)
			continue
		}
		o.reconcileTenant(p, t)
	}
	p.want(p.now.Add(tenantDriftInterval))
	return p.wish, nil
}

// tenantWork is the state of one tenant during a pass.
type tenantWork struct {
	o    *tenantOperator
	p    *tenantPass
	obj  *structs.CustomObject
	spec *tenantSpec

	owned     map[string]tenantResourceRef
	adopted   []tenantResourceRef
	conflicts []string
	failures  []string
	changed   []string

	// restoreFailures are the resources the tenant took over, no longer
	// wants and could not give back as it found them.
	restoreFailures []string
}

// owns reports whether the tenant created the resource.
func (w *tenantWork) owns(ref tenantResourceRef) bool {
	_, ok := w.owned[ref.key()]
	return ok
}

// othersResource reports whether the pass knows another tenant as the owner
// of the resource, and which one.
func (p *tenantPass) othersResource(tenant string, ref tenantResourceRef) (string, bool) {
	owner, ok := p.tenants[ref.key()]
	return owner, ok && owner != tenant
}

// take decides whether the tenant may write a resource of its name: its
// own, a new one, or an existing one it adopts. It records a conflict
// otherwise. original is what the resource holds now in the fields a tenant
// writes, nil when it does not exist: an adoption saves it, to give the
// resource back as it was.
//
// A resource has one tenant: what a tenant takes is the pass's knowledge at
// once, so a tenant later in the same pass can't take it too, and the owner
// the pass knows is asked before the tenant's own list. Of two tenants
// that list one resource (which this and the write checks should never let
// happen) the one the pass does not know as its owner gives it up.
//
// A resource of the tenant's list is the tenant's also when the list says
// it is pending and it exists now: the tenant wrote its intent down before
// it created the resource, and the status write that would have said so may
// have been lost. take writes nothing: the pass records what was taken
// (recordClaims) before it writes a resource.
func (w *tenantWork) take(ref tenantResourceRef, original *tenantOriginal) bool {
	exists := original != nil
	if owner, ok := w.p.othersResource(w.obj.Name, ref); ok {
		delete(w.owned, ref.key())
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s belongs to the tenant %s", ref, owner))
		return false
	}
	if mine, ok := w.owned[ref.key()]; ok {
		mine.Pending = !exists
		if !exists {
			// What the tenant took over is gone, and the tenant makes it
			// again: from now on it is the tenant's own, and there is
			// nothing to give back.
			mine.Adopted, mine.Original = false, nil
		}
		w.owned[ref.key()] = mine
		w.p.tenants[ref.key()] = w.obj.Name
		return true
	}
	if owner, ok := w.p.releases[ref.key()]; ok {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s belongs to the release %s", ref, owner))
		return false
	}
	if exists && !slices.Contains(w.spec.Adopt, ref.Type) {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s exists and was not created by the tenant (adopt it with adopt = [%q])", ref, ref.Type))
		return false
	}
	if exists {
		// Nothing is taken over that could not be given back: what the
		// resource holds must fit into the status.
		size, total := original.size(), 0
		for _, mine := range w.owned {
			if mine.Original != nil {
				total += mine.Original.size()
			}
		}
		if size > tenantOriginalMax || total+size > tenantOriginalsMax {
			w.conflicts = append(w.conflicts, fmt.Sprintf(
				"%s can't be taken over: the tenant keeps what an adopted resource holds to give it back, "+
					"and this one holds %d bytes (at most %d for a resource, %d for all of a tenant)",
				ref, size, tenantOriginalMax, tenantOriginalsMax))
			return false
		}
		w.adopted = append(w.adopted, ref)
		ref.Original = original
	}
	ref.Pending, ref.Adopted = !exists, exists
	w.owned[ref.key()] = ref
	w.p.tenants[ref.key()] = w.obj.Name
	return true
}

// wrote records a write of a resource: created, or a change set back.
func (w *tenantWork) wrote(ref tenantResourceRef, existed bool, err error) {
	if err != nil {
		w.failures = append(w.failures, fmt.Sprintf("%s: %v", ref, err))
		w.o.srv.operatorEvent(w.obj, structs.ObjectEventTypeWarning, "WriteFailed", fmt.Sprintf("writing %s failed: %v", ref, err))
		return
	}
	reason, what := "Created", "created "
	if existed {
		reason, what = "Updated", "set back "
	}
	if mine, ok := w.owned[ref.key()]; ok && mine.Pending {
		mine.Pending = false
		w.owned[ref.key()] = mine
	}
	w.changed = append(w.changed, ref.String())
	w.o.srv.operatorEvent(w.obj, structs.ObjectEventTypeNormal, reason, what+ref.String())
	operatorCount(structs.TenantKind, "writes")
}

// reconcileTenant brings the resources of one tenant to its spec.
func (o *tenantOperator) reconcileTenant(p *tenantPass, obj *structs.CustomObject) {
	spec, err := decodeTenantSpec(obj.Name, obj.Spec)
	if err == nil {
		err = spec.validate(obj.Name)
	}
	status := &tenantStatus{}
	_ = structs.DecodeObjectPart(obj.Status, status)
	w := &tenantWork{o: o, p: p, obj: obj, spec: spec, owned: map[string]tenantResourceRef{}}
	for _, ref := range status.Resources {
		w.owned[ref.key()] = ref
	}
	// A spec the operator can't use keeps the namespace the status has: a
	// deletion waits for that namespace to empty.
	if err != nil {
		w.failures = append(w.failures, "the spec is invalid: "+err.Error())
		o.writeTenantStatus(w, status.Namespace)
		return
	}
	desired, err := spec.desired(obj.Name)
	if err != nil {
		w.failures = append(w.failures, err.Error())
		o.writeTenantStatus(w, status.Namespace)
		return
	}
	ns := spec.Namespace.Name

	// What the tenant wants is taken first and written second. Until the
	// status lists what was taken nothing is written, so a pass that stops
	// before that gives up what it took: the status lists only resources
	// the tenant may have created.
	listed := map[string]bool{}
	for key := range w.owned {
		listed[key] = true
	}
	stop := func() {
		for key := range w.owned {
			if !listed[key] {
				delete(w.owned, key)
			}
		}
		o.writeTenantStatus(w, ns)
	}
	// writes are the writes of the pass in their order; one that returns
	// false ends the pass.
	var writes []func() bool

	// The namespace first: without it nothing else of the tenant is
	// written.
	nsRef := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: ns}
	liveNS, err := p.store.NamespaceByName(p.ws, ns)
	if err != nil {
		w.failures = append(w.failures, err.Error())
		stop()
		return
	}
	if !w.take(nsRef, namespaceOriginal(liveNS)) {
		stop()
		return
	}

	// The quota before the namespace that names it. A namespace without the
	// quota its tenant asks for would let its people run without a limit,
	// so a quota that can't be the tenant's or can't be created stops the
	// pass, as a namespace that is not the tenant's does.
	quotaRef := tenantResourceRef{Type: structs.TenantResourceQuota, Name: tenantResourceName(obj.Name, "")}
	if desired.quota != nil {
		live, err := p.store.QuotaSpecByName(p.ws, quotaRef.Name)
		if err != nil {
			w.failures = append(w.failures, err.Error())
			stop()
			return
		}
		if !w.take(quotaRef, quotaOriginal(live)) {
			stop()
			return
		}
		writes = append(writes, func() bool {
			if live != nil && quotaSpecMatches(live, desired.quota) {
				return true
			}
			err := o.upsertQuota(live, desired.quota)
			w.wrote(quotaRef, live != nil, err)
			return err == nil || live != nil
		})
	}

	// The quota the namespace names is the tenant's to write only when the
	// tenant has a quota: without one the namespace keeps what it names. A
	// tenant whose quota left its spec takes it off the namespace again:
	// the namespace gets back what it named when the tenant took it over,
	// or nothing.
	manageQuota := desired.quota != nil
	if !manageQuota && liveNS != nil && liveNS.Quota == quotaRef.Name && listed[quotaRef.key()] {
		manageQuota = true
		if mine := w.owned[nsRef.key()]; mine.Adopted && mine.Original != nil && mine.Original.Quota != quotaRef.Name {
			desired.namespace.Quota = mine.Original.Quota
			if q, err := p.store.QuotaSpecByName(p.ws, mine.Original.Quota); err == nil && q == nil && spec.ReleaseWithoutRestore {
				desired.namespace.Quota = ""
			}
		}
	}

	// Nothing is written into a namespace that could not be created.
	writes = append(writes, func() bool {
		if liveNS != nil && namespaceMatches(liveNS, desired.namespace, manageQuota) {
			return true
		}
		err := o.upsertNamespace(liveNS, desired.namespace, manageQuota)
		w.wrote(nsRef, liveNS != nil, err)
		return err == nil || liveNS != nil
	})

	acls := o.srv.config.ACLEnabled
	if acls {
		for i, policy := range desired.policies {
			ref := tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: policy.Name}
			live, err := p.store.ACLPolicyByName(p.ws, policy.Name)
			if err != nil {
				w.failures = append(w.failures, err.Error())
				continue
			}
			if !w.take(ref, policyOriginal(live)) {
				continue
			}
			writes = append(writes, func() bool {
				if live == nil || live.Rules != policy.Rules || live.Description != policy.Description {
					w.wrote(ref, live != nil, o.upsertPolicy(live, policy))
				}
				return true
			})

			role := desired.roles[i]
			roleRef := tenantResourceRef{Type: structs.TenantResourceACLRole, Name: role.Name}
			liveRole, err := p.store.GetACLRoleByName(p.ws, role.Name)
			if err != nil {
				w.failures = append(w.failures, err.Error())
				continue
			}
			if !w.take(roleRef, roleOriginal(liveRole)) {
				continue
			}
			if liveRole != nil {
				w.setRoleID(roleRef, liveRole.ID)
			}
			writes = append(writes, func() bool {
				if liveRole == nil || !roleMatches(liveRole, role) {
					id, err := o.upsertRole(liveRole, role)
					if err == nil {
						w.setRoleID(roleRef, id)
					}
					w.wrote(roleRef, liveRole != nil, err)
				}
				return true
			})
		}
	}

	netRef := tenantResourceRef{Type: structs.TenantResourceNetworkPolicy, Namespace: ns, Name: structs.TenantNetworkPolicyName}
	if desired.network != nil {
		live, err := p.store.NetworkPolicyByName(p.ws, ns, desired.network.Name)
		switch {
		case err != nil:
			w.failures = append(w.failures, err.Error())
		case w.take(netRef, networkPolicyOriginal(live)):
			writes = append(writes, func() bool {
				if live == nil || !networkPolicyMatches(live, desired.network) {
					w.wrote(netRef, live != nil, o.upsertNetworkPolicy(desired.network))
				}
				return true
			})
		}
	}

	// The status first, then the resources.
	if !o.recordClaims(w, listed, ns) {
		return
	}
	for _, ref := range w.adopted {
		o.srv.operatorEvent(w.obj, structs.ObjectEventTypeNormal, "Adopted", fmt.Sprintf("took over the existing %s", ref))
	}
	for _, write := range writes {
		if !write() {
			o.writeTenantStatus(w, ns)
			return
		}
	}

	// What the tenant created and no longer wants goes; what it took over
	// and no longer wants is given back.
	wanted := map[string]bool{nsRef.key(): true}
	if desired.quota != nil {
		wanted[quotaRef.key()] = true
	}
	if acls {
		for i := range desired.policies {
			wanted[tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: desired.policies[i].Name}.key()] = true
			wanted[tenantResourceRef{Type: structs.TenantResourceACLRole, Name: desired.roles[i].Name}.key()] = true
		}
	}
	if desired.network != nil {
		wanted[netRef.key()] = true
	}
	for _, ref := range sortedForDeletion(w.owned) {
		if wanted[ref.key()] {
			continue
		}
		if !acls && (ref.Type == structs.TenantResourceACLPolicy || ref.Type == structs.TenantResourceACLRole) {
			continue
		}
		if _, ok := p.othersResource(obj.Name, ref); ok {
			// Another tenant's: not this one's to delete, or to list.
			delete(w.owned, ref.key())
			continue
		}
		if ref.Adopted {
			// On loan: given back as it was found, not deleted.
			if _, err := o.giveBack(obj, ref, spec.ReleaseWithoutRestore); err != nil {
				w.restoreFailures = append(w.restoreFailures, err.Error())
				o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "RestoreFailed",
					fmt.Sprintf("giving %s back failed: %v", ref, err))
				continue
			}
			delete(w.owned, ref.key())
			continue
		}
		if err := o.deleteResource(ref); err != nil {
			w.failures = append(w.failures, fmt.Sprintf("deleting %s: %v", ref, err))
			o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "DeleteFailed", fmt.Sprintf("deleting %s failed: %v", ref, err))
			continue
		}
		delete(w.owned, ref.key())
		o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "Deleted", "deleted "+ref.String()+": the tenant no longer has it")
	}

	if !acls && len(desired.policies) > 0 {
		o.writeTenantStatusNote(w, ns, "ACLs are off: the roles are not created")
		return
	}
	o.writeTenantStatus(w, ns)
}

// setRoleID records the ID of a role of the tenant.
func (w *tenantWork) setRoleID(ref tenantResourceRef, id string) {
	if mine, ok := w.owned[ref.key()]; ok {
		mine.ID = id
		w.owned[ref.key()] = mine
	}
}

// recordClaims writes the resources the pass took and the status does not
// list yet into the status, before any of them is written. It reports
// whether the pass may write: when the status can't be written (the object
// changed since the pass read it, or the write failed) the pass ends
// without creating anything, and the next one starts over.
func (o *tenantOperator) recordClaims(w *tenantWork, listed map[string]bool, namespace string) bool {
	fresh := false
	for key := range w.owned {
		if !listed[key] {
			fresh = true
		}
	}
	if !fresh {
		return true
	}
	next := structs.CopyObjectMap(w.obj.Status)
	if next == nil {
		next = map[string]any{}
	}
	next["resources"] = statusRefs(w.owned)
	next["namespace"] = namespace
	changed := w.obj.Copy()
	changed.Status = next
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to write what a tenant is about to create into its status", "tenant", w.obj.Name, "error", err)
		}
		w.p.want(w.p.now.Add(operatorsRetry))
		return false
	}
	w.obj = written
	return true
}

// statusRefs returns the resources of a tenant as its status lists them.
func statusRefs(owned map[string]tenantResourceRef) []any {
	refs := make([]any, 0, len(owned))
	for _, ref := range sortedRefs(owned) {
		m := map[string]any{"type": ref.Type, "name": ref.Name}
		if ref.Namespace != "" {
			m["namespace"] = ref.Namespace
		}
		if ref.ID != "" {
			m["id"] = ref.ID
		}
		if ref.Pending {
			m["pending"] = true
		}
		if ref.Adopted {
			m["adopted"] = true
		}
		if ref.Original != nil {
			if original, err := structs.EncodeObjectPart(ref.Original); err == nil {
				m["original"] = original
			}
		}
		refs = append(refs, m)
	}
	return refs
}

// writeTenantStatus writes the status of a tenant from a pass.
func (o *tenantOperator) writeTenantStatus(w *tenantWork, namespace string) {
	o.writeTenantStatusNote(w, namespace, "")
}

func (o *tenantOperator) writeTenantStatusNote(w *tenantWork, namespace, note string) {
	status := map[string]any{
		structs.ObjectStatusObservedGeneration: w.obj.Generation,
		"resources":                            statusRefs(w.owned),
	}
	if namespace != "" {
		status["namespace"] = namespace
	}
	if len(w.conflicts) > 0 {
		conflicts := make([]any, len(w.conflicts))
		for i, c := range w.conflicts {
			conflicts[i] = c
		}
		status["conflicts"] = conflicts
	}

	conditions := newConditionSet(w.obj.Status, w.p.now)
	ready := len(w.conflicts) == 0 && len(w.failures) == 0 && len(w.restoreFailures) == 0
	switch {
	case len(w.conflicts) > 0:
		conditions.set(structs.CertificateConditionReady, false, "Conflict", strings.Join(w.conflicts, "; "))
	case len(w.restoreFailures) > 0:
		conditions.set(structs.CertificateConditionReady, false, "RestoreFailed", strings.Join(w.restoreFailures, "; "))
	case len(w.failures) > 0:
		conditions.set(structs.CertificateConditionReady, false, "WriteFailed", strings.Join(w.failures, "; "))
	case note != "":
		conditions.set(structs.CertificateConditionReady, true, "RolesNotCreated", note)
	default:
		conditions.set(structs.CertificateConditionReady, true, "Reconciled", "")
	}
	conditions.set(structs.TenantConditionConflict, len(w.conflicts) > 0, "", "")
	conditions.set(structs.CertificateConditionDegraded, !ready && len(w.failures) > 0, "", "")
	restoreHelp := ""
	if len(w.restoreFailures) > 0 {
		restoreHelp = strings.Join(w.restoreFailures, "; ") + tenantRestoreHelp
	}
	conditions.set(structs.TenantConditionRestoreFailed, len(w.restoreFailures) > 0, "", restoreHelp)
	status[structs.ObjectStatusConditions] = conditions.list()
	if !ready {
		w.p.want(w.p.now.Add(operatorsRetry))
	}
	o.writeStatus(w.obj, status, nil)
}

// writeStatus writes a changed status (and, if finalizers is not nil, the
// finalizers) of a tenant. A failed write is retried by the next pass.
func (o *tenantOperator) writeStatus(obj *structs.CustomObject, status map[string]any, finalizers *[]string) *structs.CustomObject {
	normalized, err := structs.NormalizeObjectMap(status)
	if err != nil {
		o.logger.Warn("status of a tenant can't be encoded", "tenant", obj.Name, "error", err)
		return obj
	}
	if structs.ObjectMapsEqual(obj.Status, normalized) && (finalizers == nil || slices.Equal(obj.Finalizers, *finalizers)) {
		return obj
	}
	changed := obj.Copy()
	changed.Status = normalized
	if finalizers != nil {
		changed.Finalizers = slices.Clone(*finalizers)
	}
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to write the status of a tenant", "tenant", obj.Name, "error", err)
		}
		return obj
	}
	return written
}

// tenantRestoreHelp ends the message of the condition RestoreFailed.
const tenantRestoreHelp = ": put back what is missing, or set release_without_restore = true in the spec of the tenant to release what it took over as it is"

// finishDeletion deletes what a deleted tenant created, once its namespace
// is empty, gives back what it took over, and then lets the object go.
func (o *tenantOperator) finishDeletion(p *tenantPass, obj *structs.CustomObject) {
	if !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		return
	}
	status := &tenantStatus{}
	_ = structs.DecodeObjectPart(obj.Status, status)
	owned := map[string]tenantResourceRef{}
	for _, ref := range status.Resources {
		// What the pass knows as another tenant's is not deleted with
		// this one.
		if _, ok := p.othersResource(obj.Name, ref); !ok {
			owned[ref.key()] = ref
		}
	}
	w := &tenantWork{o: o, p: p, obj: obj, owned: owned}

	// The spec of a tenant can still be written while it is being deleted:
	// that is how release_without_restore reaches a deletion that can't
	// give a resource back.
	asIs := false
	if spec, err := decodeTenantSpec(obj.Name, obj.Spec); err == nil {
		asIs = spec.ReleaseWithoutRestore
	}

	nsRef, adoptedNS := owned[tenantResourceRef{Type: structs.TenantResourceNamespace, Name: status.Namespace}.key()]
	adoptedNS = adoptedNS && nsRef.Adopted
	var back []string
	for _, ref := range sortedForDeletion(owned) {
		if ref.Adopted {
			back = append(back, ref.String())
		}
	}
	if len(back) > 0 {
		o.reportGivenBack(obj, back, asIs)
	}

	// The tenant waits for the namespace it is going to delete, not for
	// one it gives back.
	if status.Namespace != "" && w.owns(nsRef) && !adoptedNS {
		what, err := namespaceContents(p.store, p.ws, status.Namespace)
		if err != nil {
			o.logger.Warn("failed to look into the namespace of a deleted tenant", "tenant", obj.Name, "error", err)
			p.want(p.now.Add(operatorsRetry))
			return
		}
		if what != "" {
			conditions := newConditionSet(obj.Status, p.now)
			conditions.set(structs.TenantConditionNamespaceNotEmpty, true, "NamespaceNotEmpty",
				fmt.Sprintf("the namespace %q still has %s: the tenant is deleted when it is empty", status.Namespace, what))
			next := structs.CopyObjectMap(obj.Status)
			if next == nil {
				next = map[string]any{}
			}
			next[structs.ObjectStatusConditions] = conditions.list()
			if !objectConditionTrue(obj.Status, structs.TenantConditionNamespaceNotEmpty) {
				o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "NamespaceNotEmpty",
					fmt.Sprintf("waiting for the namespace %q to be empty: it has %s", status.Namespace, what))
			}
			o.writeStatus(obj, next, nil)
			p.want(p.now.Add(tenantDeleteRetry))
			return
		}
	}

	// In the order of a deletion: an adopted namespace is given back, and
	// names its own quota again, before the quota the tenant created for
	// it is deleted.
	for _, ref := range sortedForDeletion(owned) {
		if (ref.Type == structs.TenantResourceACLPolicy || ref.Type == structs.TenantResourceACLRole) && !o.srv.config.ACLEnabled {
			delete(owned, ref.key())
			continue
		}
		if ref.Adopted {
			if _, err := o.giveBack(obj, ref, asIs); err != nil {
				o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "RestoreFailed",
					fmt.Sprintf("giving %s back failed: %v", ref, err))
				p.want(p.now.Add(operatorsRetry))
				o.writeRemaining(obj, owned, err.Error())
				return
			}
			delete(owned, ref.key())
			continue
		}
		// A namespace released as it is still names the quota the tenant
		// created for it. A quota in use can't be deleted, and taking it
		// off would not be "as it is": it stays with its namespace.
		if ref.Type == structs.TenantResourceQuota && adoptedNS && asIs {
			if ns, err := o.srv.State().NamespaceByName(nil, status.Namespace); err == nil && ns != nil && ns.Quota == ref.Name {
				o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "Released", fmt.Sprintf(
					"left %s in place: the namespace %s, released as it is, names it", ref, ns.Name))
				o.logger.Info("a deleted tenant leaves its quota in place: the namespace released as it is names it",
					"tenant", obj.Name, "quota", ref.Name, "namespace", ns.Name)
				delete(owned, ref.key())
				continue
			}
		}
		if err := o.deleteResource(ref); err != nil {
			o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "DeleteFailed", fmt.Sprintf("deleting %s failed: %v", ref, err))
			p.want(p.now.Add(operatorsRetry))
			o.writeRemaining(obj, owned, "")
			return
		}
		delete(owned, ref.key())
	}

	changed := obj.Copy()
	changed.Finalizers = slices.DeleteFunc(changed.Finalizers, func(f string) bool { return f == structs.OperatorsFinalizer })
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to remove the finalizer of a deleted tenant", "tenant", obj.Name, "error", err)
		}
		return
	}
	o.srv.finalizeObjectIfReady(written)
}

// reportGivenBack says, once, what a deleted tenant gives back and leaves in
// place: as an event of the tenant, and in the log of the servers, because
// the events of an object go with it.
func (o *tenantOperator) reportGivenBack(obj *structs.CustomObject, back []string, asIs bool) {
	how := "restored to what the tenant found"
	if asIs {
		how = "released as they are (release_without_restore)"
	}
	message := "the deleted tenant gives back what it took over, " + how + ": " + strings.Join(back, "; ") +
		". They stay: delete them by hand if they are not needed"
	if len(message) > 1000 {
		message = message[:1000]
	}
	log, err := o.srv.State().ObjectEventLog(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name)
	if err == nil && log != nil {
		for _, e := range log.Events {
			if e.Reason == "Released" && e.Message == message {
				return
			}
		}
	}
	o.logger.Info("a deleted tenant gives back what it took over", "tenant", obj.Name, "how", how, "resources", strings.Join(back, "; "))
	o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "Released", message)
}

// writeRemaining records the resources a deletion has not deleted yet, and
// with restoreFailure what keeps it from giving a resource back.
func (o *tenantOperator) writeRemaining(obj *structs.CustomObject, owned map[string]tenantResourceRef, restoreFailure string) {
	next := structs.CopyObjectMap(obj.Status)
	if next == nil {
		next = map[string]any{}
	}
	next["resources"] = statusRefs(owned)
	now := time.Now().UTC().Format(time.RFC3339)
	// The wait for the namespace is over when resources are being deleted:
	// its condition must not go on saying what the namespace had.
	if objectConditionTrue(obj.Status, structs.TenantConditionNamespaceNotEmpty) {
		next[structs.ObjectStatusConditions] = conditionsWith(next, now, structs.TenantConditionNamespaceNotEmpty, false, "", "")
	}
	if restoreFailure != "" {
		next[structs.ObjectStatusConditions] = conditionsWith(next, now, structs.TenantConditionRestoreFailed, true,
			"RestoreFailed", restoreFailure+tenantRestoreHelp)
	} else if objectConditionTrue(obj.Status, structs.TenantConditionRestoreFailed) {
		next[structs.ObjectStatusConditions] = conditionsWith(next, now, structs.TenantConditionRestoreFailed, false, "", "")
	}
	o.writeStatus(obj, next, nil)
}

// namespaceContents says what keeps a namespace from being deleted, as far
// as this region knows: its jobs, its CSI volumes, its variables and the
// objects of namespaced kinds in it, which is what the deletion of a
// namespace refuses (StateStore.DeleteNamespaces); "" when it has none.
// The jobs of the namespace in other regions are not known here: the
// deletion of the namespace asks them.
func namespaceContents(store *state.StateStore, ws memdb.WatchSet, namespace string) (string, error) {
	count := func(iter memdb.ResultIterator, err error) (int, error) {
		if err != nil {
			return 0, err
		}
		n := 0
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			n++
		}
		return n, nil
	}
	var parts []string
	add := func(what string, n int, err error) error {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
		return err
	}
	jobs, err := count(store.JobsByNamespace(ws, namespace))
	if err := add("job(s)", jobs, err); err != nil {
		return "", err
	}
	vols, err := count(store.CSIVolumesByNamespace(ws, namespace, ""))
	if err := add("volume(s)", vols, err); err != nil {
		return "", err
	}
	vars, err := count(store.GetVariablesByNamespace(ws, namespace))
	if err := add("variable(s)", vars, err); err != nil {
		return "", err
	}
	objects, err := count(store.CustomObjectsByNamespace(ws, namespace))
	if err := add("object(s)", objects, err); err != nil {
		return "", err
	}
	return strings.Join(parts, ", "), nil
}

// tenantDeleteOrder is the order in which the resources of a tenant are
// deleted: the network policy, which lives in the namespace, then the
// namespace, whose deletion can still be refused (a job registered since
// the namespace was looked into, jobs in another region); the roles and the
// policies of its people only when it is gone, so that they can still empty
// it; the quota the namespace named last.
var tenantDeleteOrder = map[string]int{
	structs.TenantResourceNetworkPolicy: 0,
	structs.TenantResourceNamespace:     1,
	structs.TenantResourceACLRole:       2,
	structs.TenantResourceACLPolicy:     3,
	structs.TenantResourceQuota:         4,
}

func sortedRefs(refs map[string]tenantResourceRef) []tenantResourceRef {
	out := slices.Collect(maps.Values(refs))
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func sortedForDeletion(refs map[string]tenantResourceRef) []tenantResourceRef {
	out := sortedRefs(refs)
	sort.SliceStable(out, func(i, j int) bool { return tenantDeleteOrder[out[i].Type] < tenantDeleteOrder[out[j].Type] })
	return out
}

// The writes, through the endpoints of the types, with the rights of the
// servers.

func (o *tenantOperator) writeRequest(namespace string) structs.WriteRequest {
	return structs.WriteRequest{Region: o.srv.Region(), Namespace: namespace, AuthToken: o.srv.getLeaderAcl()}
}

// upsertNamespace writes the namespace of a tenant. The quota it names is
// written only with quota set (see reconcileTenant).
func (o *tenantOperator) upsertNamespace(live, desired *structs.Namespace, quota bool) error {
	ns := desired.Copy()
	if live != nil {
		ns = live.Copy()
		ns.Description, ns.Meta = desired.Description, maps.Clone(desired.Meta)
		if quota {
			ns.Quota = desired.Quota
		}
	}
	ns.CreateIndex, ns.ModifyIndex, ns.Hash = 0, 0, nil
	var resp structs.GenericResponse
	return o.srv.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{ns}, WriteRequest: o.writeRequest(""),
	}, &resp)
}

// upsertQuota writes the quota of a tenant. What a tenant has no field for
// (the task defaults and the task maximum) stays as the stored quota has
// it, as upsertNamespace keeps the rest of a namespace.
func (o *tenantOperator) upsertQuota(live, desired *structs.QuotaSpec) error {
	q := desired.Copy()
	if live != nil {
		q = live.Copy()
		q.Description, q.Enforce = desired.Description, desired.Enforce
		q.Limits = desired.Copy().Limits
	}
	q.CreateIndex, q.ModifyIndex, q.Hash = 0, 0, nil
	var resp structs.GenericResponse
	return o.srv.RPC("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas: []*structs.QuotaSpec{q}, WriteRequest: o.writeRequest(""),
	}, &resp)
}

// upsertPolicy writes an ACL policy of a tenant. The job a stored policy
// is attached to, which a tenant has no field for, stays.
func (o *tenantOperator) upsertPolicy(live, p *structs.ACLPolicy) error {
	policy := *p
	if live != nil && live.JobACL != nil {
		jobACL := *live.JobACL
		policy.JobACL = &jobACL
	}
	var resp structs.GenericResponse
	return o.srv.RPC("ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies: []*structs.ACLPolicy{&policy}, WriteRequest: o.writeRequest(""),
	}, &resp)
}

func (o *tenantOperator) upsertRole(live, desired *structs.ACLRole) (string, error) {
	role := &structs.ACLRole{Name: desired.Name, Description: desired.Description, Policies: desired.Policies}
	if live != nil {
		role.ID = live.ID
	}
	var resp structs.ACLRolesUpsertResponse
	err := o.srv.RPC(structs.ACLUpsertRolesRPCMethod, &structs.ACLRolesUpsertRequest{
		ACLRoles: []*structs.ACLRole{role}, WriteRequest: o.writeRequest(""),
	}, &resp)
	if err != nil {
		return "", err
	}
	if len(resp.ACLRoles) == 1 {
		return resp.ACLRoles[0].ID, nil
	}
	return role.ID, nil
}

func (o *tenantOperator) upsertNetworkPolicy(p *structs.NetworkPolicy) error {
	var resp structs.GenericResponse
	return o.srv.RPC("NetworkPolicy.UpsertNetworkPolicies", &structs.NetworkPolicyUpsertRequest{
		NetworkPolicies: []*structs.NetworkPolicy{p.Copy()}, WriteRequest: o.writeRequest(p.Namespace),
	}, &resp)
}

// deleteResource deletes a resource of a tenant; one that is gone is not an
// error.
func (o *tenantOperator) deleteResource(ref tenantResourceRef) error {
	var resp structs.GenericResponse
	var err error
	switch ref.Type {
	case structs.TenantResourceNamespace:
		err = o.srv.RPC("Namespace.DeleteNamespaces", &structs.NamespaceDeleteRequest{
			Namespaces: []string{ref.Name}, WriteRequest: o.writeRequest(""),
		}, &resp)
	case structs.TenantResourceQuota:
		err = o.srv.RPC("Quota.DeleteQuotaSpecs", &structs.QuotaSpecDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: o.writeRequest(""),
		}, &resp)
	case structs.TenantResourceACLPolicy:
		err = o.srv.RPC("ACL.DeletePolicies", &structs.ACLPolicyDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: o.writeRequest(""),
		}, &resp)
	case structs.TenantResourceACLRole:
		id := ref.ID
		if role, lerr := o.srv.State().GetACLRoleByName(nil, ref.Name); lerr == nil && role != nil {
			id = role.ID
		} else if lerr == nil && role == nil {
			return nil
		}
		var reply structs.ACLRolesDeleteByIDResponse
		err = o.srv.RPC(structs.ACLDeleteRolesByIDRPCMethod, &structs.ACLRolesDeleteByIDRequest{
			ACLRoleIDs: []string{id}, WriteRequest: o.writeRequest(""),
		}, &reply)
	case structs.TenantResourceNetworkPolicy:
		err = o.srv.RPC("NetworkPolicy.DeleteNetworkPolicies", &structs.NetworkPolicyDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: o.writeRequest(ref.Namespace),
		}, &resp)
	default:
		return fmt.Errorf("unknown resource type %q", ref.Type)
	}
	if isNotFoundError(err) {
		return nil
	}
	return err
}

// The comparisons of what exists with what a tenant wants, on the fields
// the tenant sets.

func namespaceMatches(live, desired *structs.Namespace, quota bool) bool {
	return live.Description == desired.Description && (!quota || live.Quota == desired.Quota) &&
		maps.Equal(nilIfEmpty(live.Meta), nilIfEmpty(desired.Meta))
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func quotaSpecMatches(live, desired *structs.QuotaSpec) bool {
	enforce := func(s string) string {
		if s == "" {
			return structs.QuotaEnforceSchedule
		}
		return s
	}
	if live.Description != desired.Description || enforce(live.Enforce) != enforce(desired.Enforce) ||
		len(live.Limits) != len(desired.Limits) {
		return false
	}
	byRegion := map[string]*structs.QuotaLimit{}
	for _, l := range live.Limits {
		byRegion[l.Region] = l
	}
	intp := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	for _, d := range desired.Limits {
		l := byRegion[d.Region]
		if l == nil || l.RegionLimit == nil || d.RegionLimit == nil {
			return false
		}
		if l.RegionLimit.CPU != d.RegionLimit.CPU || l.RegionLimit.MemoryMB != d.RegionLimit.MemoryMB ||
			l.RegionLimit.MemoryMaxMB != d.RegionLimit.MemoryMaxMB ||
			intp(l.Allocations) != intp(d.Allocations) || intp(l.VariablesLimit) != intp(d.VariablesLimit) {
			return false
		}
	}
	return true
}

func roleMatches(live, desired *structs.ACLRole) bool {
	if live.Description != desired.Description || len(live.Policies) != len(desired.Policies) {
		return false
	}
	for i := range live.Policies {
		if live.Policies[i] == nil || live.Policies[i].Name != desired.Policies[i].Name {
			return false
		}
	}
	return true
}

func networkPolicyMatches(live, desired *structs.NetworkPolicy) bool {
	want := desired.Copy()
	want.Canonicalize()
	have := live.Copy()
	have.CreateIndex, have.ModifyIndex = 0, 0
	return reflect.DeepEqual(have, want)
}
