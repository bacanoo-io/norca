// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
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
// exist is marked pending, with a fingerprint of what the tenant is
// creating. Pending is a claim on the name, not ownership: a resource that
// turns up under a pending entry is the tenant's only if it holds what the
// tenant was creating (its own creation, whose status write was lost).
// Anything else was made by somebody else: a conflict, or with spec.adopt
// an adoption like any other.
//
// A resource the tenant took over (spec.adopt) is on loan: the status marks
// it adopted and keeps what the tenant found (tenant_restore.go). While the
// tenant lives it is kept like one the tenant created. It is never deleted
// by the tenant: when it leaves the spec or the tenant is deleted, what the
// tenant found is written back and the resource is released. What the tenant
// found is what the resource held when the tenant first wrote it: until that
// write the status says what the tenant is writing (Writing), and a resource
// that changed since the tenant read it, and does not hold that, is taken
// over again as it is now (review finding ATM-26b).
//
// A resource of a tenant's name that the tenant did not create is left
// alone (condition Conflict), unless spec.adopt names its type; a resource
// of a release or of another tenant is never taken. A change made to a
// resource of the tenant through its own API is set back and reported as an
// event. When the tenant is deleted, the operator waits until its namespace
// has nothing the deletion of a namespace refuses (jobs, volumes, variables,
// objects; condition NamespaceNotEmpty), then deletes what the tenant
// created: the namespace first, then its network policy, the roles and the
// policies when the namespace is gone, its quota last (tenantDeleteOrder).
//
// A pass runs in the leadership term of the server (leadershipTerm, review
// finding CON-6c). Its writes go through the endpoints, which forward them
// to the leader: a pass on a deposed leader would go on writing beside the
// pass of the new leader, and the claims of the two are not shared. So right
// before every write, deletion, status write and event the pass checks that
// its term has not ended and the server is still the Raft leader; once that
// fails it writes nothing more and ends, and the pass of the next term
// decides.

var (
	// tenantDriftInterval is how often the operator compares the resources
	// of the tenants with what they should be when nothing it watches
	// changed.
	tenantDriftInterval = 5 * time.Minute

	// tenantDeleteRetry is how long a deletion that waits for the namespace
	// to empty waits at most before it looks again.
	tenantDeleteRetry = 30 * time.Second
)

// errTenantTermEnded is the error of a write of a pass whose leadership term
// ended (CON-6c).
var errTenantTermEnded = errors.New("the leadership term of the pass ended; the next leader's pass decides")

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

	// term is the context of the leadership term the pass runs in.
	term context.Context

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

// termEnded reports whether the pass may write no more (CON-6c): its
// leadership term ended, or this server is no longer the Raft leader, which
// it notices before the term is ended.
func (o *tenantOperator) termEnded(p *tenantPass) bool {
	return p.term == nil || p.term.Err() != nil || !o.srv.IsLeader()
}

// write sends a write of the pass to an endpoint, while its term lasts.
func (o *tenantOperator) write(p *tenantPass, method string, args, reply any) error {
	if o.termEnded(p) {
		return errTenantTermEnded
	}
	return o.srv.RPC(method, args, reply)
}

// event records an event of a tenant from a pass, while its term lasts.
func (o *tenantOperator) event(p *tenantPass, obj *structs.CustomObject, eventType, reason, message string) {
	if o.termEnded(p) {
		return
	}
	o.srv.operatorEvent(obj, eventType, reason, message)
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
	// The writes of a pass are made with the leader's ACL token. There is
	// none between a revoked leadership and the one established after it
	// (a pass of the term before may still be running when the leader
	// state is rebuilt): a pass then would send an empty token,
	// be refused, and record the tenant as failed. It waits instead.
	if o.srv.getLeaderAcl() == "" {
		return time.Now().Add(operatorsSettle), nil
	}
	// Nor is there a term then: the pass waits for the term too.
	term := o.srv.releases.leadershipTerm()
	if term == nil {
		return time.Now().Add(operatorsSettle), nil
	}
	snap, err := store.Snapshot()
	if err != nil {
		return time.Time{}, err
	}
	p := &tenantPass{now: time.Now().UTC(), store: store, ws: ws, term: term}
	if p.releases, err = releaseOwners(snap); err != nil {
		return time.Time{}, err
	}
	if p.tenants, err = tenantOwners(snap); err != nil {
		return time.Time{}, err
	}
	for _, t := range tenants {
		// A pass whose term ended writes nothing more: its writes would go
		// to the new leader (CON-6c). Nor does one without its token (see
		// above): the tenants it has not come to wait for the next pass.
		if o.termEnded(p) {
			break
		}
		if o.srv.getLeaderAcl() == "" {
			p.want(time.Now().Add(operatorsSettle))
			break
		}
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

	// claimsChanged is set when the pass took a resource, or changed what
	// the status says the tenant is creating: the status is written before
	// any resource is.
	claimsChanged bool

	// claimant is the pass over the tenant in the claims registry.
	claimant resourceClaimant
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
// resource back as it was. creating is what the tenant would write into
// those fields if it created the resource now.
//
// A resource has one tenant: what a tenant takes is the pass's knowledge at
// once, so a tenant later in the same pass can't take it too, and the owner
// the pass knows is asked before the tenant's own list. Of two tenants
// that list one resource (which this and the write checks should never let
// happen) the one the pass does not know as its owner gives it up.
//
// A resource has one owner: a release operation or another pass that
// claims the resource now is writing it, so the tenant does not take it in
// this pass and keeps it if it lists it; one that a release owns is not
// the tenant's even if the tenant lists it (which the claims should never
// let happen), and the tenant gives it up without deleting it (ATM-4).
//
// A resource the list says is pending and that exists now is the tenant's
// when it holds what the list says the tenant was creating: the tenant
// wrote that down before it created the resource, and the status write that
// would have cleared pending may have been lost. One that holds anything
// else is not the tenant's creation: the entry is dropped and the resource
// is treated as any resource the tenant finds. take writes nothing: the
// pass records what was taken (recordClaims) before it writes a resource.
//
// A resource the tenant took over and has not written yet (Writing) holds
// nothing of the tenant: its check-and-set write was refused, or never sent.
// What it holds now is then what the tenant finds, so one that changed since
// the tenant read it is taken over again, as it is now, and the original of
// the earlier read is forgotten (review finding ATM-26b). The exception is
// the tenant's own write whose status write was lost: the resource holds
// what the status says the tenant was writing, which the tenant wrote down
// before the write, as it does for a creation. For an existing resource
// creating is what it will hold once the tenant wrote it.
func (w *tenantWork) take(ref tenantResourceRef, original, creating *tenantOriginal) bool {
	exists := original != nil
	if owner, ok := w.p.othersResource(w.obj.Name, ref); ok {
		delete(w.owned, ref.key())
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s belongs to the tenant %s", ref, owner))
		return false
	}
	if holder, ok := w.o.srv.releases.claims.holder(ref.key()); ok && holder.id != w.claimant.id {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s is being written by %s", ref, holder))
		return false
	}
	if owner, ok := w.p.releases[ref.key()]; ok {
		delete(w.owned, ref.key())
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s belongs to the release %s", ref, owner))
		return false
	}
	if mine, ok := w.owned[ref.key()]; ok {
		switch {
		case !exists:
			// The tenant makes it (again). What the tenant took over
			// and is gone is the tenant's own from now on, with nothing
			// to give back.
			want := creating.fingerprint()
			if !mine.Pending || mine.Creating != want || mine.Adopted {
				w.claimsChanged = true
			}
			mine.Pending, mine.Creating = true, want
			mine.Adopted, mine.Original, mine.Writing = false, nil, ""
		case mine.Pending && (mine.Creating == "" || mine.Creating != original.fingerprint()):
			// Not what the tenant was creating: somebody else made it.
			delete(w.owned, ref.key())
			ok = false
		case mine.Adopted && mine.Writing != "" && mine.Writing != original.fingerprint() &&
			mine.Original.fingerprint() != original.fingerprint():
			// Not written by the tenant, and not what the tenant read:
			// taken over again below, as it is now.
			delete(w.owned, ref.key())
			ok = false
		default:
			mine.Pending, mine.Creating = false, ""
			if mine.Adopted && mine.Writing != "" {
				switch want := creating.fingerprint(); {
				case mine.Writing == original.fingerprint():
					// The tenant's own write: the original is frozen, and
					// the status says so before the tenant writes again.
					mine.Writing = ""
					w.claimsChanged = true
				case mine.Writing != want:
					mine.Writing = want
					w.claimsChanged = true
				}
			}
		}
		if ok {
			w.owned[ref.key()] = mine
			w.p.tenants[ref.key()] = w.obj.Name
			return true
		}
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
		ref.Original, ref.Writing = original, creating.fingerprint()
	} else {
		ref.Creating = creating.fingerprint()
	}
	w.claimsChanged = true
	ref.Pending, ref.Adopted = !exists, exists
	w.owned[ref.key()] = ref
	w.p.tenants[ref.key()] = w.obj.Name
	return true
}

// claim claims the resources the tenant wants for the pass, except those the
// pass knows as a release's (take reports them), and those others claim
// (take reports them too). When the tenant does not list all of them yet, it
// reads the owners releases recorded again: a release that recorded one
// since the pass read them held its claim until it had. The resources the
// tenant lists are no release's: a release reads the owners after its claim
// too, and finds the tenant.
func (w *tenantWork) claim(refs []tenantResourceRef) error {
	claims := w.o.srv.releases.claims
	w.claimant = claims.claimant(claimTenant, w.obj.Name)
	keys := make([]string, 0, len(refs))
	again := false
	for _, ref := range refs {
		key := ref.key()
		if _, ok := w.p.releases[key]; ok {
			continue
		}
		keys = append(keys, key)
		if _, ok := w.owned[key]; !ok {
			again = true
		}
	}
	claims.claim(w.claimant, keys)
	if !again {
		return nil
	}
	snap, err := w.p.store.Snapshot()
	if err != nil {
		return err
	}
	releases, err := releaseOwners(snap)
	if err != nil {
		return err
	}
	w.p.releases = releases
	return nil
}

// mayWrite checks, right before the pass writes a resource, that the
// resource is still the tenant's to write (ATM-4): the pass claims it, and
// in the state as it is now no release owns it. It records a conflict
// otherwise. The claim makes the state check redundant on one leader; it
// counts after a change of leader, whose claims do not include this pass's.
// A pass whose term ended may write nothing (CON-6c).
func (w *tenantWork) mayWrite(ref tenantResourceRef) bool {
	if w.o.termEnded(w.p) {
		return false
	}
	if !w.o.srv.releases.claims.holds(w.claimant, ref.key()) {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s is not claimed by the pass", ref))
		return false
	}
	snap, err := w.o.srv.State().Snapshot()
	if err != nil {
		w.failures = append(w.failures, err.Error())
		return false
	}
	releases, err := releaseOwners(snap)
	if err != nil {
		w.failures = append(w.failures, err.Error())
		return false
	}
	if owner, ok := releases[ref.key()]; ok {
		w.conflicts = append(w.conflicts, fmt.Sprintf("%s belongs to the release %s", ref, owner))
		return false
	}
	return true
}

// wrote records a write of a resource: created, or a change set back.
func (w *tenantWork) wrote(ref tenantResourceRef, existed bool, err error) {
	if err != nil {
		w.failures = append(w.failures, fmt.Sprintf("%s: %v", ref, err))
		w.o.event(w.p, w.obj, structs.ObjectEventTypeWarning, "WriteFailed", fmt.Sprintf("writing %s failed: %v", ref, err))
		return
	}
	reason, what := "Created", "created "
	if existed {
		reason, what = "Updated", "set back "
	}
	if mine, ok := w.owned[ref.key()]; ok && (mine.Pending || mine.Writing != "") {
		// Created, or written for the first time since the tenant took it
		// over: from now on the original of an adopted resource is frozen.
		mine.Pending, mine.Creating, mine.Writing = false, "", ""
		w.owned[ref.key()] = mine
	}
	w.changed = append(w.changed, ref.String())
	w.o.event(w.p, w.obj, structs.ObjectEventTypeNormal, reason, what+ref.String())
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

	// The pass claims what the tenant wants before it reads who owns it
	// (resource_claims.go, review finding ATM-4), and gives the claims up
	// when it ends: by then the status lists what the tenant took.
	acls := o.srv.config.ACLEnabled
	nsRef := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: ns}
	quotaRef := tenantResourceRef{Type: structs.TenantResourceQuota, Name: tenantResourceName(obj.Name, "")}
	netRef := tenantResourceRef{Type: structs.TenantResourceNetworkPolicy, Namespace: ns, Name: structs.TenantNetworkPolicyName}
	wants := []tenantResourceRef{nsRef}
	if desired.quota != nil {
		wants = append(wants, quotaRef)
	}
	if acls {
		for i := range desired.policies {
			wants = append(wants,
				tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: desired.policies[i].Name},
				tenantResourceRef{Type: structs.TenantResourceACLRole, Name: desired.roles[i].Name})
		}
	}
	if desired.network != nil {
		wants = append(wants, netRef)
	}
	err = w.claim(wants)
	defer o.srv.releases.claims.release(w.claimant)
	if err != nil {
		w.failures = append(w.failures, err.Error())
		stop()
		return
	}

	// The namespace first: without it nothing else of the tenant is
	// written.
	liveNS, err := p.store.NamespaceByName(p.ws, ns)
	if err != nil {
		w.failures = append(w.failures, err.Error())
		stop()
		return
	}
	// The quota the namespace names is the tenant's to write only when the
	// tenant has a quota: without one the namespace keeps what it names. A
	// tenant whose quota left its spec takes it off the namespace again
	// (below). What an existing namespace will hold once the tenant wrote it
	// is therefore the quota it names now, unless the tenant writes that.
	manageQuota := desired.quota != nil || liveNS != nil && liveNS.Quota == quotaRef.Name && listed[quotaRef.key()]
	nsWriting := namespaceOriginal(desired.namespace)
	if liveNS != nil && !manageQuota {
		nsWriting.Quota = liveNS.Quota
	}
	if !w.take(nsRef, namespaceOriginal(liveNS), nsWriting) {
		stop()
		return
	}

	// The quota before the namespace that names it. A namespace without the
	// quota its tenant asks for would let its people run without a limit,
	// so a quota that can't be the tenant's or can't be created stops the
	// pass, as a namespace that is not the tenant's does.
	if desired.quota != nil {
		live, err := p.store.QuotaSpecByName(p.ws, quotaRef.Name)
		if err != nil {
			w.failures = append(w.failures, err.Error())
			stop()
			return
		}
		if !w.take(quotaRef, quotaOriginal(live), quotaOriginal(desired.quota)) {
			stop()
			return
		}
		writes = append(writes, func() bool {
			if live != nil && quotaSpecMatches(live, desired.quota) {
				return true
			}
			o.beforeWrite(quotaRef)
			if !w.mayWrite(quotaRef) {
				return false
			}
			err := o.upsertQuota(p, live, desired.quota)
			w.wrote(quotaRef, live != nil, err)
			// A quota changed or created by somebody else since the
			// read stops the pass: the next one decides whose it is.
			return err == nil || live != nil && !structs.IsIndexConflictError(err)
		})
	}

	// A tenant whose quota left its spec takes it off the namespace again:
	// the namespace gets back what it named when the tenant took it over,
	// or nothing. (A namespace the tenant has not written named the quota
	// when the tenant took it over: it gets nothing, as nsWriting says.)
	if desired.quota == nil && manageQuota {
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
		o.beforeWrite(nsRef)
		if !w.mayWrite(nsRef) {
			return false
		}
		err := o.upsertNamespace(p, liveNS, desired.namespace, manageQuota)
		w.wrote(nsRef, liveNS != nil, err)
		// A namespace changed or created by somebody else since the
		// read stops the pass: the next one decides whose it is.
		return err == nil || liveNS != nil && !structs.IsIndexConflictError(err)
	})

	if acls {
		for i, policy := range desired.policies {
			ref := tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: policy.Name}
			live, err := p.store.ACLPolicyByName(p.ws, policy.Name)
			if err != nil {
				w.failures = append(w.failures, err.Error())
				continue
			}
			if !w.take(ref, policyOriginal(live), policyOriginal(policy)) {
				continue
			}
			writes = append(writes, func() bool {
				if live == nil || live.Rules != policy.Rules || live.Description != policy.Description {
					o.beforeWrite(ref)
					if !w.mayWrite(ref) {
						return false
					}
					w.wrote(ref, live != nil, o.upsertPolicy(p, live, policy))
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
			// A role of the name with another ID than the one the tenant
			// recorded is not the tenant's: its role was deleted, and
			// somebody else created this one (review finding ATM-7). The
			// tenant no longer lists it and treats it as any role it finds.
			//
			// A role the tenant took over that no longer has the name (it
			// was renamed by hand, or deleted) is given back first, by the
			// ID the tenant recorded and under the name it has now: only
			// then does the tenant forget it, and make a role of its own or
			// take the one that has the name (review finding ATM-26c). One
			// that can't be given back keeps its entry, and the tenant
			// makes no role of the name until it can be (RestoreFailed).
			if mine, ok := w.owned[roleRef.key()]; ok && mine.ID != "" && (liveRole == nil || liveRole.ID != mine.ID) {
				if mine.Adopted {
					if _, err := o.giveBack(p, obj, mine, spec.ReleaseWithoutRestore); err != nil {
						w.restoreFailures = append(w.restoreFailures, err.Error())
						o.event(p, obj, structs.ObjectEventTypeWarning, "RestoreFailed",
							fmt.Sprintf("giving %s back failed: %v", mine, err))
						continue
					}
					delete(w.owned, roleRef.key())
				} else if liveRole != nil {
					delete(w.owned, roleRef.key())
				}
			}
			if !w.take(roleRef, roleOriginal(liveRole), roleOriginal(role)) {
				continue
			}
			if liveRole != nil {
				w.setRoleID(roleRef, liveRole.ID)
			}
			writes = append(writes, func() bool {
				if liveRole == nil || !roleMatches(liveRole, role) {
					o.beforeWrite(roleRef)
					if !w.mayWrite(roleRef) {
						return false
					}
					id, err := o.upsertRole(p, liveRole, role)
					if err == nil {
						w.setRoleID(roleRef, id)
					}
					w.wrote(roleRef, liveRole != nil, err)
				}
				return true
			})
		}
	}

	if desired.network != nil {
		live, err := p.store.NetworkPolicyByName(p.ws, ns, desired.network.Name)
		switch {
		case err != nil:
			w.failures = append(w.failures, err.Error())
		case w.take(netRef, networkPolicyOriginal(live), networkPolicyOriginal(desired.network)):
			writes = append(writes, func() bool {
				if live == nil || !networkPolicyMatches(live, desired.network) {
					o.beforeWrite(netRef)
					if !w.mayWrite(netRef) {
						return false
					}
					w.wrote(netRef, live != nil, o.upsertNetworkPolicy(p, live, desired.network))
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
		o.event(p, w.obj, structs.ObjectEventTypeNormal, "Adopted", fmt.Sprintf("took over the existing %s", ref))
	}
	for _, write := range writes {
		// A pass whose term ended stops at once, writing nothing more,
		// its status neither (CON-6c).
		if o.termEnded(p) {
			return
		}
		if !write() {
			o.writeTenantStatus(w, ns)
			return
		}
	}

	// What the tenant created and no longer wants goes; what it took over
	// and no longer wants is given back.
	wanted := map[string]bool{}
	for _, ref := range wants {
		wanted[ref.key()] = true
	}
	for _, ref := range sortedForDeletion(w.owned) {
		if o.termEnded(p) {
			return
		}
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
			if _, err := o.giveBack(p, obj, ref, spec.ReleaseWithoutRestore); err != nil {
				w.restoreFailures = append(w.restoreFailures, err.Error())
				o.event(p, obj, structs.ObjectEventTypeWarning, "RestoreFailed",
					fmt.Sprintf("giving %s back failed: %v", ref, err))
				continue
			}
			delete(w.owned, ref.key())
			continue
		}
		owner, writer, err := o.releaseHas(w.claimant, ref)
		switch {
		case err != nil:
			w.failures = append(w.failures, fmt.Sprintf("deleting %s: %v", ref, err))
			continue
		case writer != "":
			// Deleted in a later pass, if it is still the tenant's then.
			w.conflicts = append(w.conflicts, fmt.Sprintf("%s is being written by %s", ref, writer))
			continue
		case owner != "":
			delete(w.owned, ref.key())
			o.releaseOwnsEvent(p, obj, ref, owner)
			continue
		}
		if err := o.deleteResource(p, ref); err != nil {
			w.failures = append(w.failures, fmt.Sprintf("deleting %s: %v", ref, err))
			o.event(p, obj, structs.ObjectEventTypeWarning, "DeleteFailed", fmt.Sprintf("deleting %s failed: %v", ref, err))
			continue
		}
		delete(w.owned, ref.key())
		o.event(p, obj, structs.ObjectEventTypeNormal, "Deleted", "deleted "+ref.String()+": the tenant no longer has it")
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
// without creating anything, and the next one starts over. A pass whose
// term ended writes nothing (CON-6c).
func (o *tenantOperator) recordClaims(w *tenantWork, listed map[string]bool, namespace string) bool {
	if o.termEnded(w.p) {
		return false
	}
	fresh := w.claimsChanged
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
		if ref.Creating != "" {
			m["creating"] = ref.Creating
		}
		if ref.Adopted {
			m["adopted"] = true
		}
		if ref.Original != nil {
			if original, err := structs.EncodeObjectPart(ref.Original); err == nil {
				m["original"] = original
			}
		}
		if ref.Writing != "" {
			m["writing"] = ref.Writing
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
	o.writeStatus(w.p, w.obj, status, nil)
}

// writeStatus writes a changed status (and, if finalizers is not nil, the
// finalizers) of a tenant. A failed write is retried by the next pass. A
// pass whose term ended writes no status: the next term's pass writes it
// (CON-6c).
func (o *tenantOperator) writeStatus(p *tenantPass, obj *structs.CustomObject, status map[string]any, finalizers *[]string) *structs.CustomObject {
	if o.termEnded(p) {
		return obj
	}
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
		o.reportGivenBack(p, obj, back, asIs)
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
				o.event(p, obj, structs.ObjectEventTypeWarning, "NamespaceNotEmpty",
					fmt.Sprintf("waiting for the namespace %q to be empty: it has %s", status.Namespace, what))
			}
			o.writeStatus(p, obj, next, nil)
			p.want(p.now.Add(tenantDeleteRetry))
			return
		}
	}

	// In the order of a deletion: an adopted namespace is given back, and
	// names its own quota again, before the quota the tenant created for
	// it is deleted. What the tenant deletes it claims first (releaseHas).
	claims := o.srv.releases.claims
	claimant := claims.claimant(claimTenant, obj.Name)
	defer claims.release(claimant)
	for _, ref := range sortedForDeletion(owned) {
		// A deletion whose term ended stops at once: the next term's pass
		// finishes it (CON-6c).
		if o.termEnded(p) {
			return
		}
		if (ref.Type == structs.TenantResourceACLPolicy || ref.Type == structs.TenantResourceACLRole) && !o.srv.config.ACLEnabled {
			delete(owned, ref.key())
			continue
		}
		if ref.Adopted {
			if _, err := o.giveBack(p, obj, ref, asIs); err != nil {
				o.event(p, obj, structs.ObjectEventTypeWarning, "RestoreFailed",
					fmt.Sprintf("giving %s back failed: %v", ref, err))
				p.want(p.now.Add(operatorsRetry))
				o.writeRemaining(p, obj, owned, err.Error())
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
				o.event(p, obj, structs.ObjectEventTypeNormal, "Released", fmt.Sprintf(
					"left %s in place: the namespace %s, released as it is, names it", ref, ns.Name))
				o.logger.Info("a deleted tenant leaves its quota in place: the namespace released as it is names it",
					"tenant", obj.Name, "quota", ref.Name, "namespace", ns.Name)
				delete(owned, ref.key())
				continue
			}
		}
		owner, writer, err := o.releaseHas(claimant, ref)
		switch {
		case err != nil || writer != "":
			// Deleted in a later pass, if it is still the tenant's then.
			if err != nil {
				o.logger.Warn("failed to read the owners of a resource of a deleted tenant", "tenant", obj.Name, "resource", ref.String(), "error", err)
			}
			p.want(p.now.Add(operatorsRetry))
			o.writeRemaining(p, obj, owned, "")
			return
		case owner != "":
			delete(owned, ref.key())
			o.releaseOwnsEvent(p, obj, ref, owner)
			continue
		}
		if err := o.deleteResource(p, ref); err != nil {
			o.event(p, obj, structs.ObjectEventTypeWarning, "DeleteFailed", fmt.Sprintf("deleting %s failed: %v", ref, err))
			p.want(p.now.Add(operatorsRetry))
			o.writeRemaining(p, obj, owned, "")
			return
		}
		delete(owned, ref.key())
	}

	if o.termEnded(p) {
		return
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

// releaseHas checks, right before a tenant deletes a resource it lists,
// whether a release has it (review finding ATM-13): writer is the release
// operation (or the pass over another tenant) that claims it now, owner the
// release that lists it in its status, in the state as it is now. The
// claimant of the tenant claims the resource first, so that no release
// operation takes it while the tenant deletes it, and reads the owners
// after that (resource_claims.go). The claims and the write checks should
// never let a resource be both a tenant's and a release's; this keeps the
// tenant from deleting it if it is.
func (o *tenantOperator) releaseHas(claimant resourceClaimant, ref tenantResourceRef) (owner, writer string, err error) {
	if others := o.srv.releases.claims.claim(claimant, []string{ref.key()}); len(others) > 0 {
		return "", others[ref.key()].String(), nil
	}
	snap, err := o.srv.State().Snapshot()
	if err != nil {
		return "", "", err
	}
	releases, err := releaseOwners(snap)
	if err != nil {
		return "", "", err
	}
	return releases[ref.key()], "", nil
}

// releaseOwnsEvent reports a resource a tenant listed and did not delete
// because a release owns it: the tenant no longer lists it.
func (o *tenantOperator) releaseOwnsEvent(p *tenantPass, obj *structs.CustomObject, ref tenantResourceRef, owner string) {
	o.event(p, obj, structs.ObjectEventTypeWarning, "NotDeleted",
		fmt.Sprintf("did not delete %s: it belongs to the release %s; the tenant no longer lists it", ref, owner))
}

// reportGivenBack says, once, what a deleted tenant gives back and leaves in
// place: as an event of the tenant, and in the log of the servers, because
// the events of an object go with it.
func (o *tenantOperator) reportGivenBack(p *tenantPass, obj *structs.CustomObject, back []string, asIs bool) {
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
	o.event(p, obj, structs.ObjectEventTypeNormal, "Released", message)
}

// writeRemaining records the resources a deletion has not deleted yet, and
// with restoreFailure what keeps it from giving a resource back.
func (o *tenantOperator) writeRemaining(p *tenantPass, obj *structs.CustomObject, owned map[string]tenantResourceRef, restoreFailure string) {
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
	o.writeStatus(p, obj, next, nil)
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
// deleted: the namespace first, because its deletion can still be refused
// (something written into it since the tenant looked, jobs in another
// region) and a refused deletion must take nothing of the tenant with it;
// then the network policy that isolated it (the deletion of a namespace
// leaves its network policies, and they can be deleted without it); the
// roles and the policies of its people only when the namespace is gone, so
// that they can still empty it; the quota the namespace named last.
var tenantDeleteOrder = map[string]int{
	structs.TenantResourceNamespace:     0,
	structs.TenantResourceNetworkPolicy: 1,
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
// servers. Each is a check-and-set on what the tenant read (live): a create
// only while the resource does not exist, a change only while it has the
// modify index of live. What somebody else wrote since the read is not
// overwritten; the write fails with a conflict (structs.IsIndexConflictError)
// and the next pass, reading again, decides.

func (o *tenantOperator) writeRequest(namespace string) structs.WriteRequest {
	return structs.WriteRequest{Region: o.srv.Region(), Namespace: namespace, AuthToken: o.srv.getLeaderAcl()}
}

// upsertNamespace writes the namespace of a tenant. The quota it names is
// written only with quota set (see reconcileTenant).
func (o *tenantOperator) upsertNamespace(p *tenantPass, live, desired *structs.Namespace, quota bool) error {
	ns := desired.Copy()
	if live != nil {
		ns = live.Copy()
		ns.Description, ns.Meta = desired.Description, maps.Clone(desired.Meta)
		if quota {
			ns.Quota = desired.Quota
		}
	}
	var expected uint64
	if live != nil {
		expected = live.ModifyIndex
	}
	ns.CreateIndex, ns.ModifyIndex, ns.Hash = 0, 0, nil
	var resp structs.GenericResponse
	return o.write(p, "Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:          []*structs.Namespace{ns},
		ExpectedModifyIndex: map[string]uint64{ns.Name: expected},
		WriteRequest:        o.writeRequest(""),
	}, &resp)
}

// upsertQuota writes the quota of a tenant. What a tenant has no field for
// (the task defaults and the task maximum) stays as the stored quota has
// it, as upsertNamespace keeps the rest of a namespace.
func (o *tenantOperator) upsertQuota(p *tenantPass, live, desired *structs.QuotaSpec) error {
	q := desired.Copy()
	if live != nil {
		q = live.Copy()
		q.Description, q.Enforce = desired.Description, desired.Enforce
		q.Limits = desired.Copy().Limits
	}
	var expected uint64
	if live != nil {
		expected = live.ModifyIndex
	}
	q.CreateIndex, q.ModifyIndex, q.Hash = 0, 0, nil
	var resp structs.GenericResponse
	return o.write(p, "Quota.UpsertQuotaSpecs", &structs.QuotaSpecUpsertRequest{
		Quotas:              []*structs.QuotaSpec{q},
		ExpectedModifyIndex: map[string]uint64{q.Name: expected},
		WriteRequest:        o.writeRequest(""),
	}, &resp)
}

// upsertPolicy writes an ACL policy of a tenant. The job a stored policy
// is attached to, which a tenant has no field for, stays.
func (o *tenantOperator) upsertPolicy(pass *tenantPass, live, p *structs.ACLPolicy) error {
	policy := *p
	var expected uint64
	if live != nil {
		expected = live.ModifyIndex
	}
	if live != nil && live.JobACL != nil {
		jobACL := *live.JobACL
		policy.JobACL = &jobACL
	}
	var resp structs.GenericResponse
	return o.write(pass, "ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies:            []*structs.ACLPolicy{&policy},
		ExpectedModifyIndex: map[string]uint64{policy.Name: expected},
		WriteRequest:        o.writeRequest(""),
	}, &resp)
}

// upsertRole writes an ACL role of a tenant and returns its ID. A role that
// does not exist is created without an ID, which the endpoint and the state
// store refuse while a role of the name exists; a change is made to the role
// of the ID of live, only while it has the modify index of live (review
// finding ATM-26).
func (o *tenantOperator) upsertRole(p *tenantPass, live, desired *structs.ACLRole) (string, error) {
	role := &structs.ACLRole{Name: desired.Name, Description: desired.Description, Policies: desired.Policies}
	var expected map[string]uint64
	if live != nil {
		role.ID = live.ID
		expected = map[string]uint64{role.Name: live.ModifyIndex}
	}
	var resp structs.ACLRolesUpsertResponse
	err := o.write(p, structs.ACLUpsertRolesRPCMethod, &structs.ACLRolesUpsertRequest{
		ACLRoles:            []*structs.ACLRole{role},
		ExpectedModifyIndex: expected,
		WriteRequest:        o.writeRequest(""),
	}, &resp)
	if err != nil {
		return "", err
	}
	if len(resp.ACLRoles) == 1 {
		return resp.ACLRoles[0].ID, nil
	}
	return role.ID, nil
}

func (o *tenantOperator) upsertNetworkPolicy(pass *tenantPass, live, p *structs.NetworkPolicy) error {
	var expected uint64
	if live != nil {
		expected = live.ModifyIndex
	}
	var resp structs.GenericResponse
	return o.write(pass, "NetworkPolicy.UpsertNetworkPolicies", &structs.NetworkPolicyUpsertRequest{
		NetworkPolicies:     []*structs.NetworkPolicy{p.Copy()},
		ExpectedModifyIndex: map[string]uint64{structs.NetworkPolicyIndexKey(p.Namespace, p.Name): expected},
		WriteRequest:        o.writeRequest(p.Namespace),
	}, &resp)
}

// beforeWrite runs the test hook tenantBeforeWrite, if set.
func (o *tenantOperator) beforeWrite(ref tenantResourceRef) {
	if o.srv.config != nil && o.srv.config.tenantBeforeWrite != nil {
		o.srv.config.tenantBeforeWrite(ref)
	}
}

// beforeDelete runs the test hook tenantBeforeDelete, if set.
func (o *tenantOperator) beforeDelete(ref tenantResourceRef) {
	if o.srv.config != nil && o.srv.config.tenantBeforeDelete != nil {
		o.srv.config.tenantBeforeDelete(ref)
	}
}

// resourceExists reports whether a resource of a tenant is in the state.
func (o *tenantOperator) resourceExists(ref tenantResourceRef) (bool, error) {
	store := o.srv.State()
	switch ref.Type {
	case structs.TenantResourceNamespace:
		out, err := store.NamespaceByName(nil, ref.Name)
		return out != nil, err
	case structs.TenantResourceQuota:
		out, err := store.QuotaSpecByName(nil, ref.Name)
		return out != nil, err
	case structs.TenantResourceACLPolicy:
		out, err := store.ACLPolicyByName(nil, ref.Name)
		return out != nil, err
	case structs.TenantResourceACLRole:
		// The role the tenant recorded, by its ID (tenantRoleID).
		if ref.ID != "" {
			out, err := store.GetACLRoleByID(nil, ref.ID)
			return out != nil, err
		}
		out, err := store.GetACLRoleByName(nil, ref.Name)
		return out != nil, err
	case structs.TenantResourceNetworkPolicy:
		out, err := store.NetworkPolicyByName(nil, ref.Namespace, ref.Name)
		return out != nil, err
	}
	return false, fmt.Errorf("unknown resource type %q", ref.Type)
}

// tenantRoleID returns the ID of the ACL role a tenant created, "" if it no
// longer exists. The role is the one of the ID the tenant recorded, never
// the role that has its name now: that one may have been created by
// somebody else after the tenant's was deleted (review finding ATM-7). A
// role the tenant was creating when the status was written (pending, no ID
// recorded yet) is the role of the name only if it holds what the tenant
// was creating, as take decides.
func (o *tenantOperator) tenantRoleID(ref tenantResourceRef) (string, error) {
	store := o.srv.State()
	if ref.ID != "" {
		role, err := store.GetACLRoleByID(nil, ref.ID)
		if err != nil || role == nil {
			return "", err
		}
		return role.ID, nil
	}
	role, err := store.GetACLRoleByName(nil, ref.Name)
	if err != nil || role == nil {
		return "", err
	}
	if ref.Pending && ref.Creating != "" && ref.Creating == roleOriginal(role).fingerprint() {
		return role.ID, nil
	}
	return "", nil
}

// deleteResource deletes a resource of a tenant; one that is gone is not an
// error.
//
// Whether the resource is gone is read from the state after the call, never
// from the answer: the text of a refusal can hold names somebody else chose
// (a job, a volume), so it can't tell "not found" from "refused", and an
// answer without an error is not trusted either. What still exists was not
// deleted, whatever the endpoint said.
func (o *tenantOperator) deleteResource(p *tenantPass, ref tenantResourceRef) error {
	o.beforeDelete(ref)
	var resp structs.GenericResponse
	var err error
	switch ref.Type {
	case structs.TenantResourceNamespace:
		err = o.write(p, "Namespace.DeleteNamespaces", &structs.NamespaceDeleteRequest{
			Namespaces: []string{ref.Name}, WriteRequest: o.writeRequest(""),
		}, &resp)
	case structs.TenantResourceQuota:
		err = o.write(p, "Quota.DeleteQuotaSpecs", &structs.QuotaSpecDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: o.writeRequest(""),
		}, &resp)
	case structs.TenantResourceACLPolicy:
		err = o.write(p, "ACL.DeletePolicies", &structs.ACLPolicyDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: o.writeRequest(""),
		}, &resp)
	case structs.TenantResourceACLRole:
		id, lerr := o.tenantRoleID(ref)
		switch {
		case lerr != nil:
			return lerr
		case id == "":
			// The tenant's role is gone; a role of its name is not the
			// tenant's.
			return nil
		}
		ref.ID = id
		var reply structs.ACLRolesDeleteByIDResponse
		err = o.write(p, structs.ACLDeleteRolesByIDRPCMethod, &structs.ACLRolesDeleteByIDRequest{
			ACLRoleIDs: []string{id}, WriteRequest: o.writeRequest(""),
		}, &reply)
	case structs.TenantResourceNetworkPolicy:
		err = o.write(p, "NetworkPolicy.DeleteNetworkPolicies", &structs.NetworkPolicyDeleteRequest{
			Names: []string{ref.Name}, WriteRequest: o.writeRequest(ref.Namespace),
		}, &resp)
	default:
		return fmt.Errorf("unknown resource type %q", ref.Type)
	}
	exists, lookupErr := o.resourceExists(ref)
	switch {
	case lookupErr != nil:
		return errors.Join(err, lookupErr)
	case !exists:
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("the deletion of %s was answered without an error, but it still exists", ref)
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
