// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtlsproxy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// The inbound listener of a sidecar authorizes connections with an Envoy
// network RBAC filter whose policies are derived from the service intentions
// of the namespace of the service. The filter has the action ALLOW, so a
// connection no policy matches is denied: the default deny of the api/mtls
// library.
//
// The intentions are evaluated like api/mtls.Evaluate: the most specific
// matching intention decides (exact destination, then exact source
// namespace, then exact source service) and at equal specificity deny wins.
// Envoy RBAC has no notion of precedence, so every allow intention becomes
// one policy that matches its source unless a deny intention that is at
// least as specific also matches:
//
//	allow(I) = match(I) AND NOT (match(D1) OR match(D2) ...)
//
// for the deny intentions Dn with precedence >= precedence(I). A connection
// is allowed exactly when such a policy matches, which is when the deciding
// intention is an allow.
//
// The source of a connection is identified by the peer certificate: its
// namespace by the URI SAN (spiffe://<td>/ns/<namespace>/job/...), its
// services by the DNS SANs <service>.service.<namespace>.nomad, which the
// servers write for every mtls service of the group of the workload. Envoy
// matches principal_name against the URI SAN and every DNS SAN.

// Principal is an Envoy config.rbac.v3.Principal (the subset used here).
type Principal struct {
	Any           bool           `json:"any,omitempty"`
	Authenticated *Authenticated `json:"authenticated,omitempty"`
	AndIDs        *PrincipalSet  `json:"and_ids,omitempty"`
	OrIDs         *PrincipalSet  `json:"or_ids,omitempty"`
	NotID         *Principal     `json:"not_id,omitempty"`
}

// PrincipalSet is an Envoy config.rbac.v3.Principal.Set.
type PrincipalSet struct {
	IDs []*Principal `json:"ids"`
}

// Authenticated is an Envoy config.rbac.v3.Principal.Authenticated. Without
// a principal name it matches every peer with a verified certificate.
type Authenticated struct {
	PrincipalName *StringMatcher `json:"principal_name,omitempty"`
}

// StringMatcher is an Envoy type.matcher.v3.StringMatcher.
type StringMatcher struct {
	Exact      string        `json:"exact,omitempty"`
	SafeRegex  *RegexMatcher `json:"safe_regex,omitempty"`
	IgnoreCase bool          `json:"ignore_case,omitempty"`
}

// RegexMatcher is an Envoy type.matcher.v3.RegexMatcher (RE2).
type RegexMatcher struct {
	Regex string `json:"regex"`
}

// Policy is an Envoy config.rbac.v3.Policy allowing every permission to its
// principals.
type Policy struct {
	Permissions []map[string]any `json:"permissions"`
	Principals  []*Principal     `json:"principals"`
}

// intentionPrecedence returns how specific an intention is, as in api/mtls.
func intentionPrecedence(i *structs.ServiceIntention) int {
	score := 0
	if i.Destination != structs.ServiceIntentionWildcard {
		score += 4
	}
	if sourceNamespace(i) != structs.ServiceIntentionWildcard {
		score += 2
	}
	if i.Source != structs.ServiceIntentionWildcard {
		score++
	}
	return score
}

func sourceNamespace(i *structs.ServiceIntention) string {
	if i.SourceNamespace == "" {
		return i.Namespace
	}
	return i.SourceNamespace
}

// applicable returns the intentions of the destination service, sorted by
// precedence and key so the generated configuration is stable.
func applicable(intentions []*structs.ServiceIntention, namespace, service string) []*structs.ServiceIntention {
	var out []*structs.ServiceIntention
	for _, i := range intentions {
		if i == nil || i.Namespace != namespace {
			continue
		}
		if i.Destination != structs.ServiceIntentionWildcard && !strings.EqualFold(i.Destination, service) {
			continue
		}
		out = append(out, i)
	}
	sort.SliceStable(out, func(a, b int) bool {
		pa, pb := intentionPrecedence(out[a]), intentionPrecedence(out[b])
		if pa != pb {
			return pa > pb
		}
		return out[a].Key() < out[b].Key()
	})
	return out
}

// sourcePrincipal returns the principal matching the sources of an
// intention.
func sourcePrincipal(i *structs.ServiceIntention) *Principal {
	ns := sourceNamespace(i)
	var parts []*Principal
	if ns != structs.ServiceIntentionWildcard {
		parts = append(parts, &Principal{Authenticated: &Authenticated{PrincipalName: &StringMatcher{
			SafeRegex: &RegexMatcher{Regex: "^spiffe://[^/]+/ns/" + regexp.QuoteMeta(ns) + "/job/[^/]+/group/[^/]+$"},
		}}})
	}
	if i.Source != structs.ServiceIntentionWildcard {
		source := strings.ToLower(i.Source)
		if ns != structs.ServiceIntentionWildcard {
			parts = append(parts, &Principal{Authenticated: &Authenticated{PrincipalName: &StringMatcher{
				Exact:      fmt.Sprintf("%s.service.%s.%s", source, ns, structs.DefaultMTLSDNSDomain),
				IgnoreCase: true,
			}}})
		} else {
			// The servers only write DNS names of the namespace of the
			// workload, so any namespace label is the source namespace.
			parts = append(parts, &Principal{Authenticated: &Authenticated{PrincipalName: &StringMatcher{
				SafeRegex: &RegexMatcher{Regex: "(?i)^" + regexp.QuoteMeta(source) + `\.service\.[^.]+\.` +
					regexp.QuoteMeta(structs.DefaultMTLSDNSDomain) + "$"},
			}}})
		}
	}
	switch len(parts) {
	case 0:
		return &Principal{Authenticated: &Authenticated{}}
	case 1:
		return parts[0]
	default:
		return &Principal{AndIDs: &PrincipalSet{IDs: parts}}
	}
}

// disjoint returns whether no peer can match both intentions: their exact
// source namespaces differ.
func disjoint(a, b *structs.ServiceIntention) bool {
	na, nb := sourceNamespace(a), sourceNamespace(b)
	return na != structs.ServiceIntentionWildcard && nb != structs.ServiceIntentionWildcard && na != nb
}

// RBACPolicies translates the intentions of a namespace into the ALLOW
// policies of the inbound listener of a destination service. An empty
// result denies every connection.
func RBACPolicies(intentions []*structs.ServiceIntention, namespace, service string) map[string]*Policy {
	intentions = applicable(intentions, namespace, service)
	policies := make(map[string]*Policy)
	for _, allow := range intentions {
		if allow.Action != structs.ServiceIntentionActionAllow {
			continue
		}
		score := intentionPrecedence(allow)
		var denies []*Principal
		for _, deny := range intentions {
			if deny.Action != structs.ServiceIntentionActionDeny || intentionPrecedence(deny) < score || disjoint(allow, deny) {
				continue
			}
			denies = append(denies, sourcePrincipal(deny))
		}
		principal := sourcePrincipal(allow)
		if len(denies) > 0 {
			var not *Principal
			if len(denies) == 1 {
				not = denies[0]
			} else {
				not = &Principal{OrIDs: &PrincipalSet{IDs: denies}}
			}
			principal = &Principal{AndIDs: &PrincipalSet{IDs: []*Principal{principal, {NotID: not}}}}
		}
		name := fmt.Sprintf("%03d %s/%s -> %s", len(policies), sourceNamespace(allow), allow.Source, allow.Destination)
		policies[name] = &Policy{
			Permissions: []map[string]any{{"any": true}},
			Principals:  []*Principal{principal},
		}
	}
	return policies
}
