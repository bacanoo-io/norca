// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The namespaces of the DNS server take patterns, so a new tenant namespace
// needs no change of every client, and Consul names reach a namespace with
// Consul's own label (<namespace>.ns, optionally <datacenter>.dc) (review
// finding PLD-3). The list still decides which namespaces are answered;
// the first entry is the namespace of names without one and must be a name.
func TestDNS_NamespacePatterns(t *testing.T) {
	ci.Parallel(t)

	s := newTestServer(t, Config{
		Namespaces:               []string{"default", "team-*"},
		ConsulCompat:             true,
		ConsulDatacenterIsRegion: true,
		TTL:                      time.Second,
	}, structs.Topology{Region: "eu", Datacenter: "dc1"})
	s.catalog.set("eu",
		instance("w1", "default", "web", "10.0.0.1", 8080),
		instance("a1", "team-a", "api", "10.0.1.1", 8080),
		instance("b1", "team-b", "api", "10.0.2.1", 8080),
		instance("s1", "secret", "db", "10.0.3.1", 5432))
	s.catalog.set("us", instance("u1", "team-a", "api", "10.9.0.1", 8080))

	// .norca names: every namespace of the pattern, nothing else
	must.Eq(t, []string{"10.0.1.1"}, answers(s.query(t, "api.service.team-a.norca", dns.TypeA)))
	must.Eq(t, []string{"10.0.2.1"}, answers(s.query(t, "api.service.team-b.norca", dns.TypeA)))
	must.Eq(t, dns.RcodeRefused, s.query(t, "db.service.secret.norca", dns.TypeA).Rcode)
	must.Eq(t, []string{"10.0.0.1"}, answers(s.query(t, "web.service.norca", dns.TypeA)))

	// .consul names: the default namespace, or one named with .ns
	must.Eq(t, []string{"10.0.0.1"}, answers(s.query(t, "web.service.consul", dns.TypeA)))
	must.Eq(t, []string{"10.0.1.1"}, answers(s.query(t, "api.service.team-a.ns.consul", dns.TypeA)))
	must.Eq(t, []string{"10.9.0.1"}, answers(s.query(t, "api.service.team-a.ns.us.dc.consul", dns.TypeA)))
	// the .dc label alone stays in the default namespace
	must.Eq(t, dns.RcodeNameError, s.query(t, "api.service.us.dc.consul", dns.TypeA).Rcode)
	must.Eq(t, dns.RcodeRefused, s.query(t, "db.service.secret.ns.consul", dns.TypeA).Rcode)
	must.NotEq(t, dns.RcodeSuccess, s.query(t, "api.service.team-a.xx.consul", dns.TypeA).Rcode)
}

// The first namespace is the one of names without a namespace: a pattern
// there is refused when the server starts.
func TestDNS_FirstNamespaceIsAName(t *testing.T) {
	ci.Parallel(t)
	for _, first := range []string{"*", "team-*", "[ab]"} {
		_, err := New(testlog.HCLogger(t), Config{Bind: "127.0.0.1:0", Domain: "norca", Namespaces: []string{first}}, nil)
		must.ErrorContains(t, err, "the first of namespaces", must.Sprint(first))
	}
	_, err := New(testlog.HCLogger(t), Config{Bind: "127.0.0.1:0", Domain: "norca", Namespaces: []string{"default", "[bad"}}, nil)
	must.ErrorContains(t, err, "namespaces")
}
