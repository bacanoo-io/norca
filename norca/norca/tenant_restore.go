// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"sort"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Taking a resource over is a loan. When a tenant adopts a resource it saves
// what it is about to overwrite in its status (the original), keeps the
// resource to its spec while it lives, and gives it back as it found it:
// when the tenant is deleted, or when the resource leaves its spec, the
// original is written back and the resource is released (dropped from the
// status). A resource that can't be given back (what it named is gone)
// keeps the tenant from letting it go, with the condition RestoreFailed,
// until it can be, or until spec.release_without_restore tells the tenant
// to release it as it is.

const (
	// tenantOriginalMax is the size, as JSON, the original of one adopted
	// resource may have, and tenantOriginalsMax the size of all originals
	// of a tenant together: the status of an object is limited
	// (structs.MaxCustomObjectSize, with the spec), and a resource whose
	// original can't be kept is not adopted, because it could not be given
	// back.
	tenantOriginalMax  = 32 << 10
	tenantOriginalsMax = 128 << 10
)

// tenantOriginal is what an adopted resource held, in the fields a tenant
// writes, before the tenant wrote it. Which fields are used depends on the
// type of the resource.
type tenantOriginal struct {
	// Description of a namespace, a quota, an ACL policy or an ACL role.
	Description string `json:"description"`

	// Meta and Quota of a namespace.
	Meta  map[string]string `json:"meta,omitempty"`
	Quota string            `json:"quota,omitempty"`

	// Enforce and Limits (by region) of a quota.
	Enforce string                       `json:"enforce,omitempty"`
	Limits  map[string]*tenantQuotaLimit `json:"limits,omitempty"`

	// Rules of an ACL policy.
	Rules string `json:"rules,omitempty"`

	// Policies are the names of the policies an ACL role links.
	Policies []string `json:"policies,omitempty"`

	// NetworkPolicy is a network policy as it was, without its indexes.
	NetworkPolicy *structs.NetworkPolicy `json:"network_policy,omitempty"`

	// CreateIndex is the create index of the resource this was read from, 0
	// for what a tenant wants to create. It is not what the resource held
	// and is not kept with it (nor in the fingerprint and the size): an
	// adoption records it beside the original (tenantResourceRef).
	CreateIndex uint64 `json:"-"`
}

// size returns the size of the original in the status.
func (o *tenantOriginal) size() int {
	buf, err := json.Marshal(o)
	if err != nil {
		return tenantOriginalsMax + 1
	}
	return len(buf)
}

// fingerprint identifies what a resource holds in the fields a tenant
// writes: equal for a resource the tenant created and for what the tenant
// set out to create. Values the endpoints fill in are made equal first.
func (o *tenantOriginal) fingerprint() string {
	if o == nil {
		return ""
	}
	c := *o
	if c.Enforce == "" && c.Limits != nil {
		c.Enforce = structs.QuotaEnforceSchedule
	}
	if c.NetworkPolicy != nil {
		c.NetworkPolicy = c.NetworkPolicy.Copy()
		c.NetworkPolicy.Canonicalize()
		c.NetworkPolicy.CreateIndex, c.NetworkPolicy.ModifyIndex = 0, 0
	}
	buf, err := json.Marshal(&c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:16])
}

func namespaceOriginal(ns *structs.Namespace) *tenantOriginal {
	if ns == nil {
		return nil
	}
	return &tenantOriginal{Description: ns.Description, Meta: maps.Clone(nilIfEmpty(ns.Meta)), Quota: ns.Quota,
		CreateIndex: ns.CreateIndex}
}

func quotaOriginal(q *structs.QuotaSpec) *tenantOriginal {
	if q == nil {
		return nil
	}
	out := &tenantOriginal{Description: q.Description, Enforce: q.Enforce, CreateIndex: q.CreateIndex}
	for _, l := range q.Limits {
		if l == nil {
			continue
		}
		limit := &tenantQuotaLimit{}
		if l.RegionLimit != nil {
			limit.CPU, limit.Memory, limit.MemoryMax = l.RegionLimit.CPU, l.RegionLimit.MemoryMB, l.RegionLimit.MemoryMaxMB
		}
		if l.Allocations != nil {
			v := *l.Allocations
			limit.Allocations = &v
		}
		if l.VariablesLimit != nil {
			v := *l.VariablesLimit
			limit.VariablesMB = &v
		}
		if out.Limits == nil {
			out.Limits = map[string]*tenantQuotaLimit{}
		}
		out.Limits[l.Region] = limit
	}
	return out
}

func policyOriginal(p *structs.ACLPolicy) *tenantOriginal {
	if p == nil {
		return nil
	}
	return &tenantOriginal{Description: p.Description, Rules: p.Rules, CreateIndex: p.CreateIndex}
}

func roleOriginal(r *structs.ACLRole) *tenantOriginal {
	if r == nil {
		return nil
	}
	out := &tenantOriginal{Description: r.Description, CreateIndex: r.CreateIndex}
	for _, link := range r.Policies {
		if link != nil {
			out.Policies = append(out.Policies, link.Name)
		}
	}
	return out
}

func networkPolicyOriginal(np *structs.NetworkPolicy) *tenantOriginal {
	if np == nil {
		return nil
	}
	saved := np.Copy()
	saved.CreateIndex, saved.ModifyIndex = 0, 0
	return &tenantOriginal{Description: np.Description, NetworkPolicy: saved, CreateIndex: np.CreateIndex}
}

// quotaLimits returns the limits of a quota from the limits by region.
func quotaLimits(byRegion map[string]*tenantQuotaLimit) []*structs.QuotaLimit {
	regions := make([]string, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	var out []*structs.QuotaLimit
	for _, r := range regions {
		l := byRegion[r]
		if l == nil {
			l = &tenantQuotaLimit{}
		}
		limit := &structs.QuotaLimit{
			Region:      r,
			RegionLimit: &structs.Resources{CPU: l.CPU, MemoryMB: l.Memory, MemoryMaxMB: l.MemoryMax},
		}
		if l.Allocations != nil {
			v := *l.Allocations
			limit.Allocations = &v
		}
		if l.VariablesMB != nil {
			v := *l.VariablesMB
			limit.VariablesLimit = &v
		}
		out = append(out, limit)
	}
	return out
}

// How a resource a tenant took over was given back.
const (
	tenantGivenBackRestored = "restored"
	tenantGivenBackAsIs     = "as it is"
	tenantGivenBackGone     = "gone"

	// tenantGivenBackUnwritten is a resource the tenant never wrote: there
	// is nothing of the tenant in it to take out.
	tenantGivenBackUnwritten = "not written"
)

// giveBack gives a resource the tenant took over back: it writes what the
// tenant saved at the adoption over what the tenant wrote, or with asIs
// leaves the resource as it is. It returns how the resource was given back.
// An error means the resource can't be given back as it was found; the
// tenant then keeps it, and the error says what is missing.
func (o *tenantOperator) giveBack(p *tenantPass, obj *structs.CustomObject, ref tenantResourceRef, asIs bool) (string, error) {
	how, err := o.restore(p, obj, ref, asIs)
	if err != nil {
		return "", err
	}
	switch how {
	case tenantGivenBackRestored:
		o.event(p, obj, structs.ObjectEventTypeNormal, "Restored",
			"gave "+ref.String()+" back as the tenant found it")
	case tenantGivenBackAsIs:
		o.event(p, obj, structs.ObjectEventTypeNormal, "Released",
			"released "+ref.String()+" as it is, without restoring what the tenant found")
	case tenantGivenBackUnwritten:
		o.event(p, obj, structs.ObjectEventTypeNormal, "Released",
			"released "+ref.String()+": the tenant had not written it")
	default:
		o.event(p, obj, structs.ObjectEventTypeNormal, "Released",
			"released "+ref.String()+": it no longer exists")
	}
	return how, nil
}

func (o *tenantOperator) restore(p *tenantPass, obj *structs.CustomObject, ref tenantResourceRef, asIs bool) (string, error) {
	store := o.srv.State()
	orig := ref.Original
	// Without a saved original there is nothing to write back.
	asIs = asIs || orig == nil

	// Nor is there when the tenant has not written the resource (Writing):
	// what the tenant read may be older than what the resource holds, and
	// is not written over it (review finding ATM-26b). A resource that
	// holds what the tenant was writing was written by the tenant.
	if ref.Writing != "" {
		live, err := o.liveOriginal(ref)
		if err != nil {
			return "", err
		}
		if live != nil && !ref.replaced(live.CreateIndex) && live.fingerprint() != ref.Writing {
			return tenantGivenBackUnwritten, nil
		}
	}

	// The resource is the one the tenant took over: a namespace, a quota,
	// an ACL policy or a network policy that has its name now with another
	// create index was created by somebody else after the tenant's was
	// deleted. The tenant's is gone, and what it found in it is not written
	// onto the other one (review finding ATM-26e).
	switch ref.Type {
	case structs.TenantResourceNamespace:
		live, err := store.NamespaceByName(nil, ref.Name)
		if err != nil {
			return "", err
		}
		if live == nil || ref.replaced(live.CreateIndex) {
			return tenantGivenBackGone, nil
		}
		if asIs {
			return tenantGivenBackAsIs, nil
		}
		want := &structs.Namespace{Name: ref.Name, Description: orig.Description, Meta: maps.Clone(orig.Meta), Quota: live.Quota}
		// The quota reference is the tenant's write only while the
		// namespace names the quota of the tenant.
		if live.Quota == tenantResourceName(obj.Name, "") && orig.Quota != live.Quota {
			if orig.Quota != "" {
				q, err := store.QuotaSpecByName(nil, orig.Quota)
				if err != nil {
					return "", err
				}
				if q == nil {
					return "", fmt.Errorf("%s named the quota %q when the tenant took it over, which no longer exists", ref, orig.Quota)
				}
			}
			want.Quota = orig.Quota
		}
		if namespaceMatches(live, want, true) {
			return tenantGivenBackRestored, nil
		}
		if err := o.upsertNamespace(p, live, want, true); err != nil {
			return "", fmt.Errorf("%s: %v", ref, err)
		}

	case structs.TenantResourceQuota:
		live, err := store.QuotaSpecByName(nil, ref.Name)
		if err != nil {
			return "", err
		}
		if live == nil || ref.replaced(live.CreateIndex) {
			return tenantGivenBackGone, nil
		}
		if asIs {
			return tenantGivenBackAsIs, nil
		}
		want := &structs.QuotaSpec{Name: ref.Name, Description: orig.Description, Enforce: orig.Enforce, Limits: quotaLimits(orig.Limits)}
		if quotaSpecMatches(live, want) {
			return tenantGivenBackRestored, nil
		}
		if err := o.upsertQuota(p, live, want); err != nil {
			return "", fmt.Errorf("%s: %v", ref, err)
		}

	case structs.TenantResourceACLPolicy:
		live, err := store.ACLPolicyByName(nil, ref.Name)
		if err != nil {
			return "", err
		}
		if live == nil || ref.replaced(live.CreateIndex) {
			return tenantGivenBackGone, nil
		}
		if asIs {
			return tenantGivenBackAsIs, nil
		}
		if live.Rules == orig.Rules && live.Description == orig.Description {
			return tenantGivenBackRestored, nil
		}
		want := &structs.ACLPolicy{Name: ref.Name, Description: orig.Description, Rules: orig.Rules}
		if err := o.upsertPolicy(p, live, want); err != nil {
			return "", fmt.Errorf("%s: %v", ref, err)
		}

	case structs.TenantResourceACLRole:
		// The role the tenant took over is the one of the ID it recorded
		// (tenantRoleID): a role that has its name now may have been
		// created by somebody else after the tenant's was deleted, and is
		// not the tenant's to write (review finding ATM-14).
		id, err := o.tenantRoleID(ref)
		if err != nil {
			return "", err
		}
		if id == "" {
			return tenantGivenBackGone, nil
		}
		live, err := store.GetACLRoleByID(nil, id)
		if err != nil {
			return "", err
		}
		if live == nil {
			return tenantGivenBackGone, nil
		}
		if asIs {
			return tenantGivenBackAsIs, nil
		}
		// The tenant writes the description and the policies of a role,
		// never its name: the role keeps the name it has.
		want := &structs.ACLRole{Name: live.Name, Description: orig.Description}
		for _, name := range orig.Policies {
			policy, err := store.ACLPolicyByName(nil, name)
			if err != nil {
				return "", err
			}
			if policy == nil {
				return "", fmt.Errorf("%s linked the policy %q when the tenant took it over, which no longer exists", ref, name)
			}
			want.Policies = append(want.Policies, &structs.ACLRolePolicyLink{Name: name})
		}
		if roleMatches(live, want) {
			return tenantGivenBackRestored, nil
		}
		// A check-and-set on the role as read here: a rename in between
		// is not written back (review finding ATM-26).
		o.beforeWrite(ref)
		if _, err := o.upsertRole(p, live, want); err != nil {
			return "", fmt.Errorf("%s: %v", ref, err)
		}

	case structs.TenantResourceNetworkPolicy:
		live, err := store.NetworkPolicyByName(nil, ref.Namespace, ref.Name)
		if err != nil {
			return "", err
		}
		if live == nil || ref.replaced(live.CreateIndex) {
			return tenantGivenBackGone, nil
		}
		if asIs || orig.NetworkPolicy == nil {
			return tenantGivenBackAsIs, nil
		}
		// The namespace of the policy is deleted before it when the
		// tenant created the namespace: there is nothing to restore a
		// policy into.
		if ns, err := store.NamespaceByName(nil, ref.Namespace); err != nil {
			return "", err
		} else if ns == nil {
			return tenantGivenBackAsIs, nil
		}
		want := orig.NetworkPolicy.Copy()
		want.Name, want.Namespace = ref.Name, ref.Namespace
		if networkPolicyMatches(live, want) {
			return tenantGivenBackRestored, nil
		}
		if err := o.upsertNetworkPolicy(p, live, want); err != nil {
			return "", fmt.Errorf("%s: %v", ref, err)
		}

	default:
		return "", fmt.Errorf("unknown resource type %q", ref.Type)
	}
	return tenantGivenBackRestored, nil
}

// liveOriginal returns what a resource of a tenant holds now in the fields a
// tenant writes, nil when it does not exist. The ACL role is the one of the
// ID the tenant recorded (tenantRoleID).
func (o *tenantOperator) liveOriginal(ref tenantResourceRef) (*tenantOriginal, error) {
	store := o.srv.State()
	switch ref.Type {
	case structs.TenantResourceNamespace:
		live, err := store.NamespaceByName(nil, ref.Name)
		return namespaceOriginal(live), err
	case structs.TenantResourceQuota:
		live, err := store.QuotaSpecByName(nil, ref.Name)
		return quotaOriginal(live), err
	case structs.TenantResourceACLPolicy:
		live, err := store.ACLPolicyByName(nil, ref.Name)
		return policyOriginal(live), err
	case structs.TenantResourceACLRole:
		id, err := o.tenantRoleID(ref)
		if err != nil || id == "" {
			return nil, err
		}
		live, err := store.GetACLRoleByID(nil, id)
		return roleOriginal(live), err
	case structs.TenantResourceNetworkPolicy:
		live, err := store.NetworkPolicyByName(nil, ref.Namespace, ref.Name)
		return networkPolicyOriginal(live), err
	}
	return nil, fmt.Errorf("unknown resource type %q", ref.Type)
}

// conditionsWith returns the conditions of a status with one of them set,
// the others as they are.
func conditionsWith(status map[string]any, now string, conditionType string, on bool, reason, message string) []any {
	value := "False"
	if on {
		value = "True"
	}
	next := map[string]any{"type": conditionType, "status": value, "lastTransitionTime": now}
	if reason != "" {
		next["reason"] = reason
	}
	if message != "" {
		if len(message) > 1000 {
			message = message[:1000]
		}
		next["message"] = message
	}
	out := []any{}
	found := false
	raw, _ := status[structs.ObjectStatusConditions].([]any)
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok || m["type"] != conditionType {
			out = append(out, item)
			continue
		}
		if m["status"] == value {
			if t, ok := m["lastTransitionTime"].(string); ok && t != "" {
				next["lastTransitionTime"] = t
			}
		}
		out = append(out, next)
		found = true
	}
	if !found {
		out = append(out, next)
	}
	return out
}
