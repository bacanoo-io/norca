// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestParseNetworkPolicyPort(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		spec string
		want *NetworkPolicyPort
		err  bool
	}{
		{spec: "5432/tcp", want: &NetworkPolicyPort{Protocol: "tcp", From: 5432, To: 5432}},
		{spec: "53", want: &NetworkPolicyPort{Protocol: "tcp", From: 53, To: 53}},
		{spec: "53/UDP", want: &NetworkPolicyPort{Protocol: "udp", From: 53, To: 53}},
		{spec: "8000-8100/sctp", want: &NetworkPolicyPort{Protocol: "sctp", From: 8000, To: 8100}},
		{spec: "db/tcp", want: &NetworkPolicyPort{Protocol: "tcp", Label: "db"}},
		{spec: "http", want: &NetworkPolicyPort{Protocol: "tcp", Label: "http"}},
		{spec: "0/tcp", err: true},
		{spec: "70000/tcp", err: true},
		{spec: "100-50/tcp", err: true},
		{spec: "80/icmp", err: true},
		{spec: "/tcp", err: true},
		{spec: "a-b/tcp", err: true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := ParseNetworkPolicyPort(tc.spec)
			if tc.err {
				must.Error(t, err)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tc.want, got)
		})
	}
	p, _ := ParseNetworkPolicyPort("8000-8100/udp")
	must.Eq(t, "8000-8100/udp", p.String())
}

func validNetworkPolicy() *NetworkPolicy {
	return &NetworkPolicy{
		Name:      "db-only-from-api",
		Namespace: "prod",
		Selector:  map[string]string{"app": "postgres"},
		Ingress: []*NetworkPolicyRule{{
			Peers: []*NetworkPolicyPeer{{Selector: map[string]string{"app": "api"}}},
			Ports: []string{"5432/tcp"},
		}},
		Egress: []*NetworkPolicyRule{{
			Peers: []*NetworkPolicyPeer{{CIDR: []string{"10.20.0.0/16"}}},
			Ports: []string{"443/tcp"},
		}},
	}
}

func TestNetworkPolicy_Validate(t *testing.T) {
	ci.Parallel(t)

	p := validNetworkPolicy()
	p.Canonicalize()
	must.Eq(t, NetworkPolicyEnforcementRequired, p.Enforcement)
	must.NoError(t, p.Validate())

	cases := map[string]func(p *NetworkPolicy){
		"bad name":             func(p *NetworkPolicy) { p.Name = "bad name" },
		"no rules":             func(p *NetworkPolicy) { p.Ingress, p.Egress = nil, nil },
		"bad enforcement":      func(p *NetworkPolicy) { p.Enforcement = "maybe" },
		"bad default deny":     func(p *NetworkPolicy) { p.DefaultDeny = []string{"sideways"} },
		"action in namespace":  func(p *NetworkPolicy) { p.Ingress[0].Action = NetworkPolicyActionDeny },
		"bad cidr":             func(p *NetworkPolicy) { p.Egress[0].Peers[0].CIDR = []string{"10.0.0.0/33"} },
		"cidr with selector":   func(p *NetworkPolicy) { p.Egress[0].Peers[0].Selector = map[string]string{"a": "b"} },
		"bad port":             func(p *NetworkPolicy) { p.Ingress[0].Ports = []string{"99999"} },
		"egress port label":    func(p *NetworkPolicy) { p.Egress[0].Ports = []string{"db/tcp"} },
		"bad peer namespace":   func(p *NetworkPolicy) { p.Ingress[0].Peers[0].Namespace = "no spaces" },
		"duplicate rule names": func(p *NetworkPolicy) { p.Ingress = append(p.Ingress, p.Ingress[0]); p.Ingress[0].Name = "x" },
		"empty selector key":   func(p *NetworkPolicy) { p.Selector = map[string]string{"": "x"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := validNetworkPolicy()
			p.Canonicalize()
			mutate(p)
			must.Error(t, p.Validate())
		})
	}

	// default_deny alone is a valid policy.
	deny := &NetworkPolicy{Name: "deny", Namespace: "prod", DefaultDeny: []string{NetworkPolicyDirectionIngress}}
	deny.Canonicalize()
	must.NoError(t, deny.Validate())
	must.True(t, deny.Isolates(NetworkPolicyDirectionIngress))
	must.False(t, deny.Isolates(NetworkPolicyDirectionEgress))
}

func TestAdminNetworkPolicy_Validate(t *testing.T) {
	ci.Parallel(t)

	p := &AdminNetworkPolicy{
		Name:       "no-metadata",
		Priority:   10,
		Namespaces: []string{"*"},
		Egress: []*NetworkPolicyRule{{
			Name:   "metadata",
			Action: NetworkPolicyActionDeny,
			Peers:  []*NetworkPolicyPeer{{CIDR: []string{"169.254.169.254/32"}}},
		}},
	}
	p.Canonicalize()
	must.NoError(t, p.Validate())

	p.Egress[0].Action = ""
	must.ErrorContains(t, p.Validate(), "invalid action")
	p.Egress[0].Action = NetworkPolicyActionPass
	p.Priority = AdminNetworkPolicyMaxPriority + 1
	must.ErrorContains(t, p.Validate(), "priority")
}

func TestNetworkPolicy_Selects(t *testing.T) {
	ci.Parallel(t)

	p := validNetworkPolicy()
	must.True(t, p.Selects("prod", map[string]string{"app": "postgres", "team": "x"}))
	must.False(t, p.Selects("prod", map[string]string{"app": "api"}))
	must.False(t, p.Selects("dev", map[string]string{"app": "postgres"}))

	p.Selector = nil
	must.True(t, p.Selects("prod", nil))

	admin := &AdminNetworkPolicy{Namespaces: []string{"team-*"}, Selector: map[string]string{"tier": "web"}}
	must.True(t, admin.Selects("team-a", map[string]string{"tier": "web"}))
	must.False(t, admin.Selects("prod", map[string]string{"tier": "web"}))
	admin.Namespaces = nil
	must.True(t, admin.Selects("prod", map[string]string{"tier": "web"}))
}

func TestNetworkPolicyMeta(t *testing.T) {
	ci.Parallel(t)

	job := &Job{Meta: map[string]string{"app": "db", "team": "a"}}
	tg := &TaskGroup{Meta: map[string]string{"app": "db-replica"}}
	must.Eq(t, map[string]string{"app": "db-replica", "team": "a"}, NetworkPolicyMeta(job, tg))
	// The job meta is not modified.
	must.Eq(t, "db", job.Meta["app"])
}

func TestNetworkPolicyPeer_Matches(t *testing.T) {
	ci.Parallel(t)

	meta := map[string]string{"app": "api"}
	must.True(t, (&NetworkPolicyPeer{}).Matches("prod", "prod", meta))
	must.False(t, (&NetworkPolicyPeer{}).Matches("prod", "dev", meta))
	must.True(t, (&NetworkPolicyPeer{Namespace: "*"}).Matches("prod", "dev", meta))
	must.True(t, (&NetworkPolicyPeer{Namespace: "dev"}).Matches("prod", "dev", meta))
	// Admin policies: an empty peer namespace matches every namespace.
	must.True(t, (&NetworkPolicyPeer{}).Matches("", "dev", meta))
	must.False(t, (&NetworkPolicyPeer{Selector: map[string]string{"app": "web"}}).Matches("prod", "prod", meta))
	// CIDR peers never match allocations.
	must.False(t, (&NetworkPolicyPeer{CIDR: []string{"10.0.0.0/8"}}).Matches("prod", "prod", meta))
}

func TestNetworkPolicyNodeCanEnforce(t *testing.T) {
	ci.Parallel(t)

	capable := &Node{Attributes: map[string]string{NetworkPolicyNftablesAttr: "true"}}
	plain := &Node{Attributes: map[string]string{}}

	ok, _ := NetworkPolicyNodeCanEnforce(capable, "bridge")
	must.True(t, ok)
	ok, _ = NetworkPolicyNodeCanEnforce(capable, "cni/flannel")
	must.True(t, ok)
	ok, reason := NetworkPolicyNodeCanEnforce(capable, "host")
	must.False(t, ok)
	must.StrContains(t, reason, "host")
	ok, _ = NetworkPolicyNodeCanEnforce(plain, "bridge")
	must.False(t, ok)
	ok, _ = NetworkPolicyNodeCanEnforce(plain, "none")
	must.True(t, ok)

	must.Eq(t, "host", NetworkPolicyGroupMode(&TaskGroup{}))
	must.Eq(t, "bridge", NetworkPolicyGroupMode(&TaskGroup{Networks: Networks{{Mode: "bridge"}}}))
}

func TestNetworkPolicy_Copy(t *testing.T) {
	ci.Parallel(t)

	p := validNetworkPolicy()
	c := p.Copy()
	c.Selector["app"] = "x"
	c.Ingress[0].Peers[0].Selector["app"] = "x"
	c.Egress[0].Ports[0] = "1/tcp"
	must.Eq(t, "postgres", p.Selector["app"])
	must.Eq(t, "api", p.Ingress[0].Peers[0].Selector["app"])
	must.Eq(t, "443/tcp", p.Egress[0].Ports[0])
}

func TestSortAdminNetworkPolicies(t *testing.T) {
	ci.Parallel(t)

	policies := []*AdminNetworkPolicy{{Name: "b", Priority: 5}, {Name: "c", Priority: 1}, {Name: "a", Priority: 5}}
	SortAdminNetworkPolicies(policies)
	must.Eq(t, "c", policies[0].Name)
	must.Eq(t, "a", policies[1].Name)
	must.Eq(t, "b", policies[2].Name)
}
