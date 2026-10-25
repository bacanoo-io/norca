// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtlsproxy

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/api/mtls"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// peer is the identity Envoy matches principals against: the URI SAN and
// the DNS SANs of the peer certificate.
type peer struct {
	uri string
	dns []string
}

func newPeer(namespace, region string, services ...string) peer {
	p := peer{uri: structs.MTLSWorkloadSPIFFEID("global.nomad", namespace, "job", "group")}
	for _, s := range services {
		p.dns = append(p.dns, structs.MTLSServiceDNSNames(s, namespace, region)...)
	}
	return p
}

// matchString evaluates an Envoy StringMatcher.
func matchString(t *testing.T, m *StringMatcher, value string) bool {
	switch {
	case m.SafeRegex != nil:
		return regexp.MustCompile(m.SafeRegex.Regex).MatchString(value)
	case m.IgnoreCase:
		return strings.EqualFold(m.Exact, value)
	default:
		return m.Exact == value
	}
}

// matchPrincipal evaluates a principal like Envoy: an authenticated
// principal name matches the URI SAN, or any DNS SAN.
func matchPrincipal(t *testing.T, p *Principal, peer peer) bool {
	switch {
	case p.Any:
		return true
	case p.Authenticated != nil:
		if p.Authenticated.PrincipalName == nil {
			return true
		}
		if matchString(t, p.Authenticated.PrincipalName, peer.uri) {
			return true
		}
		for _, name := range peer.dns {
			if matchString(t, p.Authenticated.PrincipalName, name) {
				return true
			}
		}
		return false
	case p.AndIDs != nil:
		for _, id := range p.AndIDs.IDs {
			if !matchPrincipal(t, id, peer) {
				return false
			}
		}
		return true
	case p.OrIDs != nil:
		for _, id := range p.OrIDs.IDs {
			if matchPrincipal(t, id, peer) {
				return true
			}
		}
		return false
	case p.NotID != nil:
		return !matchPrincipal(t, p.NotID, peer)
	}
	t.Fatalf("empty principal")
	return false
}

// allowed evaluates an ALLOW RBAC filter: a connection is allowed if any
// policy matches it.
func allowed(t *testing.T, policies map[string]*Policy, peer peer) bool {
	for _, policy := range policies {
		for _, p := range policy.Principals {
			if matchPrincipal(t, p, peer) {
				return true
			}
		}
	}
	return false
}

func toAPI(in []*structs.ServiceIntention) []*api.ServiceIntention {
	out := make([]*api.ServiceIntention, len(in))
	for i, x := range in {
		out[i] = &api.ServiceIntention{
			Namespace:       x.Namespace,
			Destination:     x.Destination,
			SourceNamespace: x.SourceNamespace,
			Source:          x.Source,
			Action:          x.Action,
		}
	}
	return out
}

func TestRBACPolicies_Examples(t *testing.T) {
	ci.Parallel(t)

	in := func(srcNS, src, dst, action string) *structs.ServiceIntention {
		return &structs.ServiceIntention{Namespace: "default", SourceNamespace: srcNS, Source: src, Destination: dst, Action: action}
	}
	cases := []struct {
		name       string
		intentions []*structs.ServiceIntention
		peer       peer
		allowed    bool
	}{
		{name: "default deny", peer: newPeer("default", "global", "web")},
		{
			name:       "exact allow",
			intentions: []*structs.ServiceIntention{in("default", "web", "api", "allow")},
			peer:       newPeer("default", "global", "web"),
			allowed:    true,
		},
		{
			name:       "other source",
			intentions: []*structs.ServiceIntention{in("default", "web", "api", "allow")},
			peer:       newPeer("default", "global", "batch"),
		},
		{
			name:       "other namespace",
			intentions: []*structs.ServiceIntention{in("default", "web", "api", "allow")},
			peer:       newPeer("prod", "global", "web"),
		},
		{
			name:       "case insensitive source",
			intentions: []*structs.ServiceIntention{in("default", "Web", "API", "allow")},
			peer:       newPeer("default", "global", "web"),
			allowed:    true,
		},
		{
			name: "specific allow beats wildcard deny",
			intentions: []*structs.ServiceIntention{
				in("*", "*", "*", "deny"),
				in("default", "web", "api", "allow"),
			},
			peer:    newPeer("default", "global", "web"),
			allowed: true,
		},
		{
			name: "deny wins at equal precedence through another service",
			intentions: []*structs.ServiceIntention{
				in("default", "web", "api", "allow"),
				in("default", "admin", "api", "deny"),
			},
			peer: newPeer("default", "global", "web", "admin"),
		},
		{
			name: "more specific deny",
			intentions: []*structs.ServiceIntention{
				in("default", "*", "api", "allow"),
				in("default", "web", "api", "deny"),
			},
			peer: newPeer("default", "global", "web"),
		},
		{
			name:       "wildcard namespace exact service",
			intentions: []*structs.ServiceIntention{in("*", "web", "api", "allow")},
			peer:       newPeer("prod", "eu", "web"),
			allowed:    true,
		},
		{
			name:       "intention of another destination",
			intentions: []*structs.ServiceIntention{in("default", "web", "db", "allow")},
			peer:       newPeer("default", "global", "web"),
		},
		{
			name:       "peer without services, wildcard source",
			intentions: []*structs.ServiceIntention{in("default", "*", "api", "allow")},
			peer:       newPeer("default", "global"),
			allowed:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policies := RBACPolicies(tc.intentions, "default", "api")
			must.Eq(t, tc.allowed, allowed(t, policies, tc.peer))
		})
	}
}

// TestRBACPolicies_MatchesLibrary checks the translation against the
// evaluation of the api/mtls library on random intentions and peers.
func TestRBACPolicies_MatchesLibrary(t *testing.T) {
	ci.Parallel(t)

	rng := rand.New(rand.NewSource(42))
	namespaces := []string{"default", "prod", "*"}
	services := []string{"web", "api", "db", "admin", "*"}
	pick := func(values []string) string { return values[rng.Intn(len(values))] }

	for round := 0; round < 3000; round++ {
		seen := make(map[string]bool)
		var intentions []*structs.ServiceIntention
		for n := rng.Intn(6); n > 0; n-- {
			i := &structs.ServiceIntention{
				Namespace:       pick(namespaces[:2]),
				Destination:     pick(services),
				SourceNamespace: pick(namespaces),
				Source:          pick(services),
				Action:          pick([]string{"allow", "deny"}),
			}
			if rng.Intn(4) == 0 {
				i.Source = strings.ToUpper(i.Source)
			}
			if seen[i.Key()] {
				continue
			}
			seen[i.Key()] = true
			intentions = append(intentions, i)
		}

		peerNS := pick(namespaces[:2])
		var peerServices []string
		for _, s := range services[:4] {
			if rng.Intn(3) == 0 {
				peerServices = append(peerServices, s)
			}
		}
		p := newPeer(peerNS, pick([]string{"global", "eu"}), peerServices...)
		dstNS, dst := pick(namespaces[:2]), pick(services[:4])

		want := mtls.Evaluate(toAPI(intentions), mtls.Source{Namespace: peerNS, Services: peerServices}, dstNS, dst, false).Allowed
		got := allowed(t, RBACPolicies(intentions, dstNS, dst), p)
		if want != got {
			var b strings.Builder
			for _, i := range intentions {
				fmt.Fprintf(&b, "  %s %s\n", i.Key(), i.Action)
			}
			t.Fatalf("round %d: peer %s/%v to %s/%s: library allowed=%v, rbac allowed=%v\n%s",
				round, peerNS, peerServices, dstNS, dst, want, got, b.String())
		}
	}
}
