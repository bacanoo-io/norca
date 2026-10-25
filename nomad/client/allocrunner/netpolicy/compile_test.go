// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package netpolicy

import (
	"net/netip"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// specPolicy is the example of the Kubernetes features spec.
func specPolicy() *structs.NetworkPolicy {
	return &structs.NetworkPolicy{
		Name:      "db-only-from-api",
		Namespace: "prod",
		Selector:  map[string]string{"app": "postgres"},
		Ingress: []*structs.NetworkPolicyRule{{
			Peers: []*structs.NetworkPolicyPeer{{Selector: map[string]string{"app": "api"}}},
			Ports: []string{"5432/tcp"},
		}},
		Egress: []*structs.NetworkPolicyRule{{
			Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"10.20.0.0/16"}}},
			Ports: []string{"443/tcp"},
		}},
		Enforcement: structs.NetworkPolicyEnforcementRequired,
	}
}

func TestCompile_NoRules(t *testing.T) {
	ci.Parallel(t)

	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{}})
	must.NoError(t, err)
	must.Nil(t, rs)
	must.Eq(t, "", rs.String())

	rs, err = Compile(&Input{Namespace: "prod"})
	must.NoError(t, err)
	must.Nil(t, rs)
}

func TestCompile_SpecExample(t *testing.T) {
	ci.Parallel(t)

	in := &Input{
		Namespace: "prod",
		Rules: &structs.NetworkPolicyAllocRules{
			Policies: []*structs.NetworkPolicy{specPolicy()},
			Peers: []*structs.NetworkPolicyPeerAlloc{
				{ID: "api-1", Namespace: "prod", Meta: map[string]string{"app": "api"}, Address: "172.26.64.5"},
				{ID: "api-other-ns", Namespace: "dev", Meta: map[string]string{"app": "api"}, Address: "172.26.64.6"},
				{ID: "web", Namespace: "prod", Meta: map[string]string{"app": "web"}, Address: "172.26.64.7"},
			},
		},
		NodeAddrs: []netip.Addr{netip.MustParseAddr("172.26.64.1")},
	}
	rs, err := Compile(in)
	must.NoError(t, err)
	must.NotNil(t, rs)

	expected := `table inet norca {
	counter ingress_default {
	}
	counter egress_default {
	}
	chain ingress {
		type filter hook input priority filter; policy accept;
		iifname "lo" accept
		ct state established,related accept
		ct state invalid drop
		meta l4proto ipv6-icmp icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept
		ip saddr 172.26.64.1 accept
		jump ingress_ns
	}
	chain ingress_ns {
		ip saddr 172.26.64.5 tcp dport 5432 accept comment "prod/db-only-from-api/ingress/0"
		counter name "ingress_default" drop
	}
	chain egress {
		type filter hook output priority filter; policy accept;
		oifname "lo" accept
		ct state established,related accept
		ct state invalid drop
		meta l4proto ipv6-icmp icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept
		jump egress_ns
	}
	chain egress_ns {
		ip daddr 10.20.0.0/16 tcp dport 443 accept comment "prod/db-only-from-api/egress/0"
		counter name "egress_default" drop
	}
}
`
	must.Eq(t, expected, rs.String())

	must.Len(t, 2, rs.Counters)
	must.Eq(t, &Counter{Name: "ingress_default", Direction: "ingress", Kind: "default-deny", Policy: "db-only-from-api"}, rs.Counters[0])
}

func TestCompile_EgressOnlyLeavesIngressOpen(t *testing.T) {
	ci.Parallel(t)

	p := specPolicy()
	p.Ingress = nil
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{Policies: []*structs.NetworkPolicy{p}}})
	must.NoError(t, err)
	must.Nil(t, rs.Ingress)
	must.NotNil(t, rs.Egress)
	must.True(t, rs.Egress.Isolated)
	must.Len(t, 1, rs.Egress.Allow)
}

func TestCompile_UnresolvedPeersStillIsolate(t *testing.T) {
	ci.Parallel(t)

	// No allocation of the node matches the ingress peer: the rule is
	// dropped, but being selected still means default deny.
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{
		Policies: []*structs.NetworkPolicy{specPolicy()},
	}})
	must.NoError(t, err)
	must.True(t, rs.Ingress.Isolated)
	must.Len(t, 0, rs.Ingress.Allow)
	must.Eq(t, "ingress_default", rs.Ingress.DefaultCounter)
}

func TestCompile_RulesAddUp(t *testing.T) {
	ci.Parallel(t)

	other := &structs.NetworkPolicy{
		Name:      "dns",
		Namespace: "prod",
		Egress: []*structs.NetworkPolicyRule{{
			Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"10.0.0.53/32", "fd00::53/128"}}},
			Ports: []string{"53/udp", "53/tcp"},
		}},
	}
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{
		Policies: []*structs.NetworkPolicy{specPolicy(), other},
	}})
	must.NoError(t, err)
	// Policies are evaluated in name order.
	must.Len(t, 2, rs.Egress.Allow)
	must.Eq(t, "prod/db-only-from-api/egress/0", rs.Egress.Allow[0].Origin)
	must.Eq(t, "prod/dns/egress/0", rs.Egress.Allow[1].Origin)
	must.Eq(t, []PortRange{{ProtoTCP, 53, 53}, {ProtoUDP, 53, 53}}, rs.Egress.Allow[1].Ports)
	must.Len(t, 4, rs.Egress.Allow[1].expand())
	must.Eq(t, "db-only-from-api,dns", rs.Counters[len(rs.Counters)-1].Policy)
}

func TestCompile_DefaultDenyWithoutRules(t *testing.T) {
	ci.Parallel(t)

	p := &structs.NetworkPolicy{
		Name:        "deny-ingress",
		Namespace:   "prod",
		DefaultDeny: []string{structs.NetworkPolicyDirectionIngress},
	}
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{Policies: []*structs.NetworkPolicy{p}}})
	must.NoError(t, err)
	must.NotNil(t, rs.Ingress)
	must.True(t, rs.Ingress.Isolated)
	must.Nil(t, rs.Egress)
}

func TestCompile_EmptyRuleAllowsAll(t *testing.T) {
	ci.Parallel(t)

	p := &structs.NetworkPolicy{
		Name:      "allow-all",
		Namespace: "prod",
		Ingress:   []*structs.NetworkPolicyRule{{}},
	}
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{Policies: []*structs.NetworkPolicy{p}}})
	must.NoError(t, err)
	must.Len(t, 1, rs.Ingress.Allow)
	must.Nil(t, rs.Ingress.Allow[0].Prefixes)
	must.Nil(t, rs.Ingress.Allow[0].Ports)
	must.StrContains(t, rs.String(), "\t\taccept comment \"prod/allow-all/ingress/0\"\n")
}

func TestCompile_PeerNamespaces(t *testing.T) {
	ci.Parallel(t)

	peers := []*structs.NetworkPolicyPeerAlloc{
		{ID: "a", Namespace: "prod", Meta: map[string]string{"app": "api"}, Address: "10.1.0.1"},
		{ID: "b", Namespace: "dev", Meta: map[string]string{"app": "api"}, Address: "10.1.0.2"},
		{ID: "c", Namespace: "dev", Meta: map[string]string{"app": "web"}, Address: "fd00::3"},
	}
	cases := []struct {
		name string
		peer *structs.NetworkPolicyPeer
		want []string
	}{
		{"same namespace", &structs.NetworkPolicyPeer{Selector: map[string]string{"app": "api"}}, []string{"10.1.0.1/32"}},
		{"other namespace", &structs.NetworkPolicyPeer{Namespace: "dev"}, []string{"10.1.0.2/32", "fd00::3/128"}},
		{"all namespaces", &structs.NetworkPolicyPeer{Namespace: "*", Selector: map[string]string{"app": "api"}}, []string{"10.1.0.1/32", "10.1.0.2/32"}},
		{"everything in namespace", &structs.NetworkPolicyPeer{}, []string{"10.1.0.1/32"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &structs.NetworkPolicy{
				Name: "p", Namespace: "prod",
				Ingress: []*structs.NetworkPolicyRule{{Peers: []*structs.NetworkPolicyPeer{tc.peer}}},
			}
			rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{
				Policies: []*structs.NetworkPolicy{p}, Peers: peers,
			}})
			must.NoError(t, err)
			must.Len(t, 1, rs.Ingress.Allow)
			var got []string
			for _, prefix := range rs.Ingress.Allow[0].Prefixes {
				got = append(got, prefix.String())
			}
			must.Eq(t, tc.want, got)
		})
	}
}

func TestCompile_PortLabels(t *testing.T) {
	ci.Parallel(t)

	p := &structs.NetworkPolicy{
		Name: "p", Namespace: "prod",
		Ingress: []*structs.NetworkPolicyRule{
			{Ports: []string{"db/tcp", "http", "8000-8100/udp"}},
			{Ports: []string{"missing/tcp"}},
		},
	}
	ports := structs.AllocatedPorts{
		{Label: "db", Value: 25432, To: 5432},
		{Label: "http", Value: 28080},
	}
	rs, err := Compile(&Input{Namespace: "prod", Ports: ports, Rules: &structs.NetworkPolicyAllocRules{
		Policies: []*structs.NetworkPolicy{p},
	}})
	must.NoError(t, err)
	// The rule with only an unknown label cannot match and is left out.
	must.Len(t, 1, rs.Ingress.Allow)
	must.Eq(t, []PortRange{{ProtoTCP, 5432, 5432}, {ProtoTCP, 28080, 28080}, {ProtoUDP, 8000, 8100}}, rs.Ingress.Allow[0].Ports)
	must.StrContains(t, rs.String(), "udp dport 8000-8100 accept")
}

func TestCompile_AdminLayer(t *testing.T) {
	ci.Parallel(t)

	admin := []*structs.AdminNetworkPolicy{
		{
			Name:     "late",
			Priority: 50,
			Egress: []*structs.NetworkPolicyRule{{
				Name:   "rest",
				Action: structs.NetworkPolicyActionPass,
			}},
		},
		{
			Name:     "early",
			Priority: 10,
			Egress: []*structs.NetworkPolicyRule{
				{
					Name:   "metadata",
					Action: structs.NetworkPolicyActionDeny,
					Peers:  []*structs.NetworkPolicyPeer{{CIDR: []string{"169.254.169.254/32"}}},
				},
				{
					Name:   "dns",
					Action: structs.NetworkPolicyActionAllow,
					Peers:  []*structs.NetworkPolicyPeer{{CIDR: []string{"10.0.0.53/32"}}},
					Ports:  []string{"53/udp"},
				},
			},
		},
	}
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{AdminPolicies: admin}})
	must.NoError(t, err)
	must.Nil(t, rs.Ingress)
	must.NotNil(t, rs.Egress)
	// Admin policies alone do not isolate the direction.
	must.False(t, rs.Egress.Isolated)
	must.Len(t, 3, rs.Egress.Admin)
	must.Eq(t, VerdictDrop, rs.Egress.Admin[0].Verdict)
	must.Eq(t, "egress_admin_0", rs.Egress.Admin[0].Counter)
	must.Eq(t, VerdictAccept, rs.Egress.Admin[1].Verdict)
	must.Eq(t, VerdictReturn, rs.Egress.Admin[2].Verdict)
	must.Eq(t, []*Counter{{Name: "egress_admin_0", Direction: "egress", Kind: "admin", Policy: "early", Rule: "metadata"}}, rs.Counters)

	out := rs.String()
	must.StrContains(t, out, "\t\tjump egress_admin\n")
	must.StrNotContains(t, out, "egress_ns")
	must.StrContains(t, out, `ip daddr 169.254.169.254 counter name "egress_admin_0" drop comment "admin/early/egress/metadata"`)
	must.StrContains(t, out, `return comment "admin/late/egress/rest"`)
}

func TestCompile_AdminDenyBeforeNamespaceAllow(t *testing.T) {
	ci.Parallel(t)

	// A namespace policy allowing everything cannot override an admin deny:
	// the admin chain is jumped to first.
	ns := &structs.NetworkPolicy{Name: "all", Namespace: "prod", Egress: []*structs.NetworkPolicyRule{{}}}
	admin := &structs.AdminNetworkPolicy{
		Name: "deny", Egress: []*structs.NetworkPolicyRule{{
			Action: structs.NetworkPolicyActionDeny,
			Peers:  []*structs.NetworkPolicyPeer{{CIDR: []string{"0.0.0.0/0"}}},
		}},
	}
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{
		Policies: []*structs.NetworkPolicy{ns}, AdminPolicies: []*structs.AdminNetworkPolicy{admin},
	}})
	must.NoError(t, err)
	out := rs.String()
	must.StrContains(t, out, "\t\tjump egress_admin\n\t\tjump egress_ns\n")
	must.StrContains(t, out, `meta nfproto ipv4 counter name "egress_admin_0" drop`)
}

func TestCompile_PolicyOfOtherNamespaceIgnored(t *testing.T) {
	ci.Parallel(t)

	p := specPolicy()
	p.Namespace = "dev"
	rs, err := Compile(&Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{Policies: []*structs.NetworkPolicy{p}}})
	must.NoError(t, err)
	must.Nil(t, rs)
}

func TestCompile_Deterministic(t *testing.T) {
	ci.Parallel(t)

	in := &Input{Namespace: "prod", Rules: &structs.NetworkPolicyAllocRules{
		Policies: []*structs.NetworkPolicy{specPolicy()},
		Peers: []*structs.NetworkPolicyPeerAlloc{
			{ID: "b", Namespace: "prod", Meta: map[string]string{"app": "api"}, Address: "172.26.64.9"},
			{ID: "a", Namespace: "prod", Meta: map[string]string{"app": "api"}, Address: "172.26.64.5"},
			{ID: "c", Namespace: "prod", Meta: map[string]string{"app": "api"}, Address: "172.26.64.5"},
		},
	}}
	first, err := Compile(in)
	must.NoError(t, err)
	in.Rules.Peers[0], in.Rules.Peers[1] = in.Rules.Peers[1], in.Rules.Peers[0]
	second, err := Compile(in)
	must.NoError(t, err)
	must.Eq(t, first.String(), second.String())
	// Duplicate addresses are merged.
	must.Len(t, 2, first.Ingress.Allow[0].Prefixes)
}
