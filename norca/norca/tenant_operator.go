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
// A resource of a tenant's name that the tenant did not create is left
// alone (condition Conflict), unless spec.adopt names its type; a resource
// of a release or of another tenant is never taken. A change made to a
// resource of the tenant through its own API is set back and reported as an
// event. When the tenant is deleted, the operator waits until its namespace
// has no jobs and no volumes (condition NamespaceNotEmpty), then deletes what
// the tenant created, the namespace and then its quota last.

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
	conflicts []string
	failures  []string
	changed   []string
}

// owns reports whether the tenant created the resource.
func (w *tenantWork) owns(ref tenantResourceRef) bool {
	_, ok := w.owned[ref.key()]
	return ok
}

// take decides whether the tenant may write a resource of its name: its
// own, a new one, or an existing one it adopts. It records a conflict
// otherwise.
func (w *tenantWork) take(ref tenantResourceRef, exists bool) bool {
	if w.owns(ref) {
		return true
	}
	if owner, ok := w.p.releases[ref.key()]; ok {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s belongs to the release %s", ref, owner))
		return false
	}
	if owner, ok := w.p.tenants[ref.key()]; ok && owner != w.obj.Name {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s belongs to the tenant %s", ref, owner))
		return false
	}
	if exists && !slices.Contains(w.spec.Adopt, ref.Type) {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s exists and was not created by the tenant (adopt it with adopt = [%q])", ref, ref.Type))
		return false
	}
	if exists {
		w.o.srv.operatorEvent(w.obj, structs.ObjectEventTypeNormal, "Adopted", fmt.Sprintf("took over the existing %s", ref))
	}
	w.owned[ref.key()] = ref
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
	if err != nil {
		w.failures = append(w.failures, "the spec is invalid: "+err.Error())
		o.writeTenantStatus(w, "")
		return
	}
	desired, err := spec.desired(obj.Name)
	if err != nil {
		w.failures = append(w.failures, err.Error())
		o.writeTenantStatus(w, "")
		return
	}

	// The namespace first: without it nothing else of the tenant is
	// written.
	ns := spec.Namespace.Name
	nsRef := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: ns}
	liveNS, err := p.store.NamespaceByName(p.ws, ns)
	if err != nil {
		w.failures = append(w.failures, err.Error())
		o.writeTenantStatus(w, ns)
		return
	}
	if !w.take(nsRef, liveNS != nil) {
		o.writeTenantStatus(w, ns)
		return
	}

	// The quota before the namespace that names it.
	quotaRef := tenantResourceRef{Type: structs.TenantResourceQuota, Name: tenantResourceName(obj.Name, "")}
	if desired.quota != nil {
		live, err := p.store.QuotaSpecByName(p.ws, quotaRef.Name)
		switch {
		case err != nil:
			w.failures = append(w.failures, err.Error())
		case w.take(quotaRef, live != nil):
			if live == nil || !quotaSpecMatches(live, desired.quota) {
				w.wrote(quotaRef, live != nil, o.upsertQuota(desired.quota))
			}
		default:
			desired.namespace.Quota = ""
		}
	}

	if liveNS == nil || !namespaceMatches(liveNS, desired.namespace) {
		w.wrote(nsRef, liveNS != nil, o.upsertNamespace(liveNS, desired.namespace))
	}

	acls := o.srv.config.ACLEnabled
	if acls {
		for i, policy := range desired.policies {
			ref := tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: policy.Name}
			live, err := p.store.ACLPolicyByName(p.ws, policy.Name)
			if err != nil {
				w.failures = append(w.failures, err.Error())
				continue
			}
			if !w.take(ref, live != nil) {
				continue
			}
			if live == nil || live.Rules != policy.Rules || live.Description != policy.Description {
				w.wrote(ref, live != nil, o.upsertPolicy(policy))
			}

			role := desired.roles[i]
			roleRef := tenantResourceRef{Type: structs.TenantResourceACLRole, Name: role.Name}
			liveRole, err := p.store.GetACLRoleByName(p.ws, role.Name)
			if err != nil {
				w.failures = append(w.failures, err.Error())
				continue
			}
			if !w.take(roleRef, liveRole != nil) {
				continue
			}
			if liveRole != nil {
				roleRef.ID = liveRole.ID
			}
			if liveRole == nil || !roleMatches(liveRole, role) {
				id, err := o.upsertRole(liveRole, role)
				if err == nil {
					roleRef.ID = id
				}
				w.wrote(roleRef, liveRole != nil, err)
			}
			w.owned[roleRef.key()] = roleRef
		}
	}

	netRef := tenantResourceRef{Type: structs.TenantResourceNetworkPolicy, Namespace: ns, Name: structs.TenantNetworkPolicyName}
	if desired.network != nil {
		live, err := p.store.NetworkPolicyByName(p.ws, ns, desired.network.Name)
		switch {
		case err != nil:
			w.failures = append(w.failures, err.Error())
		case w.take(netRef, live != nil):
			if live == nil || !networkPolicyMatches(live, desired.network) {
				w.wrote(netRef, live != nil, o.upsertNetworkPolicy(desired.network))
			}
		}
	}

	// What the tenant created and no longer wants goes.
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

// writeTenantStatus writes the status of a tenant from a pass.
func (o *tenantOperator) writeTenantStatus(w *tenantWork, namespace string) {
	o.writeTenantStatusNote(w, namespace, "")
}

func (o *tenantOperator) writeTenantStatusNote(w *tenantWork, namespace, note string) {
	refs := make([]any, 0, len(w.owned))
	for _, ref := range sortedRefs(w.owned) {
		m := map[string]any{"type": ref.Type, "name": ref.Name}
		if ref.Namespace != "" {
			m["namespace"] = ref.Namespace
		}
		if ref.ID != "" {
			m["id"] = ref.ID
		}
		refs = append(refs, m)
	}
	status := map[string]any{
		structs.ObjectStatusObservedGeneration: w.obj.Generation,
		"resources":                            refs,
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
	ready := len(w.conflicts) == 0 && len(w.failures) == 0
	switch {
	case len(w.conflicts) > 0:
		conditions.set(structs.CertificateConditionReady, false, "Conflict", strings.Join(w.conflicts, "; "))
	case len(w.failures) > 0:
		conditions.set(structs.CertificateConditionReady, false, "WriteFailed", strings.Join(w.failures, "; "))
	case note != "":
		conditions.set(structs.CertificateConditionReady, true, "RolesNotCreated", note)
	default:
		conditions.set(structs.CertificateConditionReady, true, "Reconciled", "")
	}
	conditions.set(structs.TenantConditionConflict, len(w.conflicts) > 0, "", "")
	conditions.set(structs.CertificateConditionDegraded, !ready && len(w.failures) > 0, "", "")
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

// finishDeletion deletes what a deleted tenant created, once its namespace
// is empty, and then lets the object go.
func (o *tenantOperator) finishDeletion(p *tenantPass, obj *structs.CustomObject) {
	if !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		return
	}
	status := &tenantStatus{}
	_ = structs.DecodeObjectPart(obj.Status, status)
	owned := map[string]tenantResourceRef{}
	for _, ref := range status.Resources {
		owned[ref.key()] = ref
	}
	w := &tenantWork{o: o, p: p, obj: obj, owned: owned}

	nsRef := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: status.Namespace}
	if status.Namespace != "" && w.owns(nsRef) {
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

	for _, ref := range sortedForDeletion(owned) {
		if (ref.Type == structs.TenantResourceACLPolicy || ref.Type == structs.TenantResourceACLRole) && !o.srv.config.ACLEnabled {
			delete(owned, ref.key())
			continue
		}
		if err := o.deleteResource(ref); err != nil {
			o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "DeleteFailed", fmt.Sprintf("deleting %s failed: %v", ref, err))
			p.want(p.now.Add(operatorsRetry))
			o.writeRemaining(obj, owned)
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

// writeRemaining records the resources a deletion has not deleted yet.
func (o *tenantOperator) writeRemaining(obj *structs.CustomObject, owned map[string]tenantResourceRef) {
	next := structs.CopyObjectMap(obj.Status)
	if next == nil {
		next = map[string]any{}
	}
	refs := []any{}
	for _, ref := range sortedRefs(owned) {
		m := map[string]any{"type": ref.Type, "name": ref.Name}
		if ref.Namespace != "" {
			m["namespace"] = ref.Namespace
		}
		if ref.ID != "" {
			m["id"] = ref.ID
		}
		refs = append(refs, m)
	}
	next["resources"] = refs
	o.writeStatus(obj, next, nil)
}

// namespaceContents says what keeps a namespace from being deleted: its
// jobs and its CSI volumes; "" when it has none.
func namespaceContents(store *state.StateStore, ws memdb.WatchSet, namespace string) (string, error) {
	jobs, err := store.JobsByNamespace(ws, namespace)
	if err != nil {
		return "", err
	}
	nJobs := 0
	for raw := jobs.Next(); raw != nil; raw = jobs.Next() {
		nJobs++
	}
	vols, err := store.CSIVolumesByNamespace(ws, namespace, "")
	if err != nil {
		return "", err
	}
	nVols := 0
	for raw := vols.Next(); raw != nil; raw = vols.Next() {
		nVols++
	}
	var parts []string
	if nJobs > 0 {
		parts = append(parts, fmt.Sprintf("%d job(s)", nJobs))
	}
	if nVols > 0 {
		parts = append(parts, fmt.Sprintf("%d volume(s)", nVols))
	}
	return strings.Join(parts, " and "), nil
}

// tenantDeleteOrder is the order in which the resources of a tenant are
// deleted: what lives in the namespace or names a policy first, the
// namespace before the quota it names.
var tenantDeleteOrder = map[string]int{
	structs.TenantResourceNetworkPolicy: 0,
	structs.TenantResourceACLRole:       1,
	structs.TenantResourceACLPolicy:     2,
	structs.TenantResourceNamespace:     3,
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

func (o *tenantOperator) upsertNamespace(live, desired *structs.Namespace) error {
	ns := desired.Copy()
	if live != nil {
		ns = live.Copy()
		ns.Description, ns.Meta, ns.Quota = desired.Description, maps.Clone(desired.Meta), desired.Quota
	}
	ns.CreateIndex, ns.ModifyIndex, ns.Hash = 0, 0, nil
	var resp structs.GenericResponse
	return o.srv.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{ns}, WriteRequest: o.writeRequest(""),
	}, &resp)
}

func (o *tenantOperator) upsertQuota(q *structs.QuotaSpec) error {
	var resp structs.GenericResponse
	return o.srv.RPC("Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas: []*structs.QuotaSpec{q.Copy()}, WriteRequest: o.writeRequest(""),
	}, &resp)
}

func (o *tenantOperator) upsertPolicy(p *structs.ACLPolicy) error {
	policy := *p
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

func namespaceMatches(live, desired *structs.Namespace) bool {
	return live.Description == desired.Description && live.Quota == desired.Quota &&
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
