// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package dns

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/discovery"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/miekg/dns"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeCatalog answers ServiceRegistration.GetService from memory. Blocking
// queries wait until the index changes or their wait time passes.
type fakeCatalog struct {
	lock    sync.Mutex
	index   uint64
	regions map[string][]*structs.ServiceRegistration
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{index: 1, regions: make(map[string][]*structs.ServiceRegistration)}
}

func (f *fakeCatalog) set(region string, regs ...*structs.ServiceRegistration) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.index++
	f.regions[region] = regs
}

func (f *fakeCatalog) rpc(_ string, args, reply any) error {
	req := args.(*structs.ServiceRegistrationByNameRequest)
	resp := reply.(*structs.ServiceRegistrationByNameResponse)

	deadline := time.Now().Add(req.MaxQueryTime)
	for {
		f.lock.Lock()
		if req.MinQueryIndex < f.index || time.Now().After(deadline) {
			break
		}
		f.lock.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	defer f.lock.Unlock()

	for _, reg := range f.regions[req.Region] {
		if reg.ServiceName == req.ServiceName && reg.Namespace == req.Namespace {
			resp.Services = append(resp.Services, reg.Copy())
		}
	}
	resp.Discovery = structs.EffectiveDiscovery(resp.Services)
	resp.Index = f.index
	return nil
}

func instance(id, ns, name, address string, port int) *structs.ServiceRegistration {
	return &structs.ServiceRegistration{
		ID: id, ServiceName: name, Namespace: ns, Datacenter: "dc1", Zone: "a",
		Address: address, Port: port, Ready: true, Health: structs.ServiceHealthPassing,
	}
}

type testServer struct {
	*Server
	catalog *fakeCatalog
	cache   *discovery.Cache
}

func newTestServer(t *testing.T, cfg Config, local structs.Topology) *testServer {
	t.Helper()
	catalog := newFakeCatalog()
	cache := discovery.NewCache(testlog.HCLogger(t), catalog.rpc, func() string { return "" })
	resolver := discovery.NewResolver(cache, func() structs.Topology { return local })

	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1:0"
	}
	if cfg.Domain == "" {
		cfg.Domain = "nomad"
	}
	if cfg.TTL == 0 {
		cfg.TTL = 5 * time.Second
	}
	srv, err := New(testlog.HCLogger(t), cfg, resolver)
	must.NoError(t, err)
	t.Cleanup(func() {
		srv.Shutdown()
		cache.Stop()
	})
	return &testServer{Server: srv, catalog: catalog, cache: cache}
}

func (s *testServer) query(t *testing.T, name string, qtype uint16) *dns.Msg {
	t.Helper()
	return s.exchange(t, "udp", name, qtype)
}

func (s *testServer) exchange(t *testing.T, network, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	c := &dns.Client{Net: network, Timeout: 5 * time.Second}
	resp, _, err := c.Exchange(m, s.Addr())
	must.NoError(t, err)
	return resp
}

func answers(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		switch r := rr.(type) {
		case *dns.A:
			out = append(out, r.A.String())
		case *dns.AAAA:
			out = append(out, r.AAAA.String())
		case *dns.CNAME:
			out = append(out, "cname:"+r.Target)
		case *dns.SRV:
			out = append(out, fmt.Sprintf("%s:%d", r.Target, r.Port))
		}
	}
	sort.Strings(out)
	return out
}

func TestDNS_ServiceRecords(t *testing.T) {
	ci.Parallel(t)

	s := newTestServer(t, Config{Namespaces: []string{"default", "prod"}, TTL: 7 * time.Second},
		structs.Topology{Region: "eu", Datacenter: "dc1", Zone: "a"})

	web1 := instance("w1", "default", "web", "10.0.0.1", 8080)
	web1.Tags = []string{"blue"}
	web2 := instance("w2", "default", "web", "10.0.0.2", 8081)
	web6 := instance("w6", "default", "web", "fd00::6", 8082)
	sick := instance("w3", "default", "web", "10.0.0.3", 8083)
	sick.Health = structs.ServiceHealthCritical
	db := instance("d1", "prod", "db", "10.1.0.1", 5432)
	ext := instance("e1", "default", "ext", "db.example.com", 5432)
	s.catalog.set("eu", web1, web2, web6, sick, db, ext)
	s.catalog.set("us", instance("u1", "prod", "db", "10.9.0.1", 5432))

	// A and AAAA of the healthy instances, with the configured TTL
	resp := s.query(t, "web.service.nomad", dns.TypeA)
	must.Eq(t, dns.RcodeSuccess, resp.Rcode)
	must.True(t, resp.Authoritative)
	must.Eq(t, []string{"10.0.0.1", "10.0.0.2"}, answers(resp))
	must.Eq(t, uint32(7), resp.Answer[0].Header().Ttl)

	resp = s.query(t, "WEB.service.nomad.", dns.TypeAAAA)
	must.Eq(t, []string{"fd00::6"}, answers(resp))

	// SRV records point to addr names, resolved in the additional section
	resp = s.query(t, "web.service.nomad", dns.TypeSRV)
	must.Eq(t, []string{"0a000001.addr.nomad.:8080", "0a000002.addr.nomad.:8081",
		"fd000000000000000000000000000006.addr.nomad.:8082"}, answers(resp))
	must.Len(t, 3, resp.Extra)

	resp = s.query(t, "0a000001.addr.nomad", dns.TypeA)
	must.Eq(t, []string{"10.0.0.1"}, answers(resp))

	// tags, in both name styles
	must.Eq(t, []string{"10.0.0.1"}, answers(s.query(t, "blue.web.service.nomad", dns.TypeA)))
	must.Eq(t, []string{"0a000001.addr.nomad.:8080"}, answers(s.query(t, "_web._blue.service.nomad", dns.TypeSRV)))
	must.Len(t, 3, s.query(t, "_web._tcp.service.nomad", dns.TypeSRV).Answer)

	// namespaces and regions
	must.Eq(t, dns.RcodeNameError, s.query(t, "db.service.nomad", dns.TypeA).Rcode)
	must.Eq(t, []string{"10.1.0.1"}, answers(s.query(t, "db.service.prod.nomad", dns.TypeA)))
	must.Eq(t, []string{"10.9.0.1"}, answers(s.query(t, "db.service.prod.us.nomad", dns.TypeA)))
	must.Eq(t, dns.RcodeRefused, s.query(t, "db.service.secret.nomad", dns.TypeA).Rcode)

	// host name addresses are CNAMEs
	must.Eq(t, []string{"cname:db.example.com."}, answers(s.query(t, "ext.service.nomad", dns.TypeA)))

	// unknown services and names
	resp = s.query(t, "nope.service.nomad", dns.TypeA)
	must.Eq(t, dns.RcodeNameError, resp.Rcode)
	must.Len(t, 1, resp.Ns)
	must.Eq(t, dns.RcodeNameError, s.query(t, "web.node.nomad", dns.TypeA).Rcode)

	// the zone apex
	must.Len(t, 1, s.query(t, "nomad", dns.TypeSOA).Answer)

	// Consul names are not answered without consul_compat, nor recursed
	must.Eq(t, dns.RcodeRefused, s.query(t, "web.service.consul", dns.TypeA).Rcode)

	// TCP works as well
	must.Eq(t, []string{"10.0.0.1", "10.0.0.2"}, answers(s.exchange(t, "tcp", "web.service.nomad", dns.TypeA)))
}

func TestDNS_ConsulCompat(t *testing.T) {
	ci.Parallel(t)

	s := newTestServer(t, Config{ConsulCompat: true, ConsulDatacenterIsRegion: true, Domain: "cluster"},
		structs.Topology{Region: "eu", Datacenter: "dc1"})
	s.catalog.set("eu", instance("w1", "default", "web", "10.0.0.1", 8080))
	s.catalog.set("us", instance("u1", "default", "web", "10.9.0.1", 8080))

	must.Eq(t, []string{"10.0.0.1"}, answers(s.query(t, "web.service.consul", dns.TypeA)))
	must.Eq(t, []string{"10.0.0.1"}, answers(s.query(t, "web.service.cluster", dns.TypeA)))
	must.Eq(t, []string{"10.9.0.1"}, answers(s.query(t, "web.service.us.consul", dns.TypeA)))
	must.Eq(t, []string{"0a000001.addr.consul.:8080"}, answers(s.query(t, "_web._tcp.service.consul", dns.TypeSRV)))
}

func TestDNS_Truncation(t *testing.T) {
	ci.Parallel(t)

	s := newTestServer(t, Config{}, structs.Topology{Region: "eu", Datacenter: "dc1"})
	var regs []*structs.ServiceRegistration
	for i := 0; i < 100; i++ {
		regs = append(regs, instance(fmt.Sprintf("w%d", i), "default", "web", fmt.Sprintf("10.0.%d.%d", i/250, i%250+1), 8080))
	}
	s.catalog.set("eu", regs...)

	resp := s.query(t, "web.service.nomad", dns.TypeSRV)
	must.True(t, resp.Truncated)
	must.Less(t, 100, len(resp.Answer))

	resp = s.exchange(t, "tcp", "web.service.nomad", dns.TypeSRV)
	must.False(t, resp.Truncated)
	must.Len(t, 100, resp.Answer)
}

func TestDNS_Recursion(t *testing.T) {
	ci.Parallel(t)

	// an upstream nameserver
	upstream := &dns.Server{Addr: "127.0.0.1:0", Net: "udp"}
	upstream.Handler = dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("192.0.2.1"),
		})
		_ = w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	must.NoError(t, err)
	upstream.PacketConn = pc
	go func() { _ = upstream.ActivateAndServe() }()
	t.Cleanup(func() { _ = upstream.Shutdown() })

	s := newTestServer(t, Config{Recursors: []string{pc.LocalAddr().String()}},
		structs.Topology{Region: "eu", Datacenter: "dc1"})

	resp := s.query(t, "example.com", dns.TypeA)
	must.Eq(t, dns.RcodeSuccess, resp.Rcode)
	must.Eq(t, []string{"192.0.2.1"}, answers(resp))

	// a dead recursor fails the query
	dead := newTestServer(t, Config{Recursors: []string{"127.0.0.1:1"}}, structs.Topology{Region: "eu"})
	must.Eq(t, dns.RcodeServerFailure, dead.query(t, "example.com", dns.TypeA).Rcode)
}

// TestDNS_TopologyAcceptance is the acceptance scenario of the plan: with
// prefer = "zone" and min_local = 2, a client in a zone gets the instances of
// its zone while at least two are healthy there, the rest of its datacenter
// when only one is left, and the first failover region when the region has
// none.
func TestDNS_TopologyAcceptance(t *testing.T) {
	ci.Parallel(t)

	s := newTestServer(t, Config{}, structs.Topology{Region: "eu", Datacenter: "dc1", Zone: "fsn1"})
	settings := &structs.ServiceDiscovery{Prefer: "zone", MinLocal: 2, OverflowFraction: 0.2, Failover: []string{"us"}}

	inst := func(id, zone, ip string, healthy bool) *structs.ServiceRegistration {
		r := instance(id, "default", "api", ip, 80)
		r.Zone = zone
		r.Discovery = settings
		if !healthy {
			r.Health = structs.ServiceHealthCritical
		}
		return r
	}

	s.catalog.set("eu",
		inst("a", "fsn1", "10.0.0.1", true), inst("b", "fsn1", "10.0.0.2", true),
		inst("c", "nbg1", "10.0.1.1", true), inst("d", "nbg1", "10.0.1.2", true))
	s.catalog.set("us", inst("u", "ash", "10.9.0.1", true))

	lookup := func() []string { return answers(s.query(t, "api.service.nomad", dns.TypeA)) }
	eventually := func(exp []string) {
		t.Helper()
		must.Wait(t, wait.InitialSuccess(
			wait.BoolFunc(func() bool { return fmt.Sprint(lookup()) == fmt.Sprint(exp) }),
			wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond),
		), must.Sprint(exp))
	}

	eventually([]string{"10.0.0.1", "10.0.0.2"})

	s.catalog.set("eu",
		inst("a", "fsn1", "10.0.0.1", true), inst("b", "fsn1", "10.0.0.2", false),
		inst("c", "nbg1", "10.0.1.1", true), inst("d", "nbg1", "10.0.1.2", true))
	eventually([]string{"10.0.0.1", "10.0.1.1", "10.0.1.2"})

	s.catalog.set("eu",
		inst("a", "fsn1", "10.0.0.1", false), inst("b", "fsn1", "10.0.0.2", false),
		inst("c", "nbg1", "10.0.1.1", false), inst("d", "nbg1", "10.0.1.2", false))
	eventually([]string{"10.9.0.1"})

	// back to health
	s.catalog.set("eu", inst("a", "fsn1", "10.0.0.1", true), inst("b", "fsn1", "10.0.0.2", true))
	eventually([]string{"10.0.0.1", "10.0.0.2"})

}
