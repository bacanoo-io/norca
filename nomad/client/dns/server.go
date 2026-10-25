// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package dns is the built-in DNS server of the client. It answers names of
// the native service catalog:
//
//	<service>.service.<domain>                       default namespace, local region
//	<service>.service.<namespace>.<domain>           the given namespace
//	<service>.service.<namespace>.<region>.<domain>  another region
//	<tag>.<service>.service...                       instances with the tag
//	_<service>._<tag|tcp|udp>.service...             RFC 2782 style SRV names
//	<hex IP>.addr.<domain>                           the targets of SRV records
//
// with A, AAAA and SRV records of the healthy instances, ordered by topology
// with regional failover. Optionally, it answers *.service.consul names as
// well (<service>.service[.<datacenter>].consul). Other names are forwarded
// to the configured recursors or refused.
package dns

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/discovery"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/miekg/dns"
)

const (
	// lookupTimeout bounds the time an answer may wait for the servers.
	lookupTimeout = 3 * time.Second

	// recursorTimeout bounds a query forwarded to one recursor.
	recursorTimeout = 2 * time.Second

	// consulDomain is the domain of the Consul-compatible names.
	consulDomain = "consul"
)

// Resolver resolves service lookups.
type Resolver interface {
	Resolve(ctx context.Context, q discovery.Query) (*discovery.Result, error)
}

// Config of the DNS server.
type Config struct {
	// Bind is the host:port to listen on, over UDP and TCP.
	Bind string

	// Domain of the native catalog, e.g. "nomad".
	Domain string

	// ConsulCompat answers *.service.consul names as well.
	ConsulCompat bool

	// ConsulDatacenterIsRegion maps the datacenter label of Consul names to
	// a Nomad region; otherwise it selects a Nomad datacenter.
	ConsulDatacenterIsRegion bool

	// Recursors are host:port nameservers for other names.
	Recursors []string

	// TTL of the answers.
	TTL time.Duration

	// Namespaces answered for; the first is the default.
	Namespaces []string
}

// Server is the DNS server.
type Server struct {
	logger   hclog.Logger
	cfg      Config
	resolver Resolver

	udp *dns.Server
	tcp *dns.Server

	udpAddr net.Addr
	tcpAddr net.Addr

	wg sync.WaitGroup
}

// New starts a DNS server.
func New(logger hclog.Logger, cfg Config, resolver Resolver) (*Server, error) {
	if len(cfg.Namespaces) == 0 {
		cfg.Namespaces = []string{structs.DefaultNamespace}
	}
	cfg.Domain = strings.Trim(strings.ToLower(cfg.Domain), ".")

	s := &Server{
		logger:   logger.Named("dns"),
		cfg:      cfg,
		resolver: resolver,
	}

	pc, err := net.ListenPacket("udp", cfg.Bind)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on udp %s: %w", cfg.Bind, err)
	}
	// listen on the same port over TCP, also when the port was chosen by the
	// system
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to listen on tcp %s: %w", cfg.Bind, err)
	}

	handler := dns.HandlerFunc(s.handle)
	s.udp = &dns.Server{PacketConn: pc, Handler: handler}
	s.tcp = &dns.Server{Listener: ln, Handler: handler}
	s.udpAddr = pc.LocalAddr()
	s.tcpAddr = ln.Addr()

	for _, srv := range []*dns.Server{s.udp, s.tcp} {
		srv := srv
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := srv.ActivateAndServe(); err != nil {
				s.logger.Error("dns server stopped", "error", err)
			}
		}()
	}

	s.logger.Info("dns server started", "udp", s.udpAddr, "tcp", s.tcpAddr, "domain", cfg.Domain)
	return s, nil
}

// Addr returns the UDP address the server listens on.
func (s *Server) Addr() string { return s.udpAddr.String() }

// Shutdown stops the server.
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.udp.ShutdownContext(ctx)
	_ = s.tcp.ShutdownContext(ctx)
	s.wg.Wait()
}

// handle answers one request.
func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	m.RecursionAvailable = len(s.cfg.Recursors) > 0

	if len(req.Question) != 1 {
		m.SetRcode(req, dns.RcodeFormatError)
		s.write(w, req, m)
		return
	}
	q := req.Question[0]
	name := strings.ToLower(q.Name)

	zone, labels, ok := s.zoneOf(name)
	if !ok {
		s.recurse(w, req)
		return
	}

	switch {
	case len(labels) == 0:
		// the apex of the zone
		if q.Qtype == dns.TypeSOA || q.Qtype == dns.TypeANY {
			m.Answer = append(m.Answer, s.soa(zone))
		} else {
			m.Ns = append(m.Ns, s.soa(zone))
		}
	case len(labels) == 2 && labels[1] == "addr":
		s.answerAddr(m, q, zone, labels)
	default:
		lookup, err := s.parseService(zone, labels)
		if err != nil {
			s.logger.Trace("invalid name", "name", name, "error", err)
			m.SetRcode(req, dns.RcodeNameError)
			m.Ns = append(m.Ns, s.soa(zone))
			break
		}
		if lookup.refused {
			m.SetRcode(req, dns.RcodeRefused)
			break
		}
		s.answerService(m, req, q, zone, lookup)
	}

	s.write(w, req, m)
}

// write sends m, truncated to what the transport allows.
func (s *Server) write(w dns.ResponseWriter, req *dns.Msg, m *dns.Msg) {
	if _, udp := w.RemoteAddr().(*net.UDPAddr); udp {
		size := dns.MinMsgSize
		if opt := req.IsEdns0(); opt != nil {
			size = int(opt.UDPSize())
			m.SetEdns0(opt.UDPSize(), false)
		}
		m.Truncate(size)
	}
	if err := w.WriteMsg(m); err != nil {
		s.logger.Debug("failed to write dns answer", "error", err)
	}
}

// zoneOf returns the zone name belongs to and its labels in front of the
// zone.
func (s *Server) zoneOf(name string) (string, []string, bool) {
	zones := []string{s.cfg.Domain}
	if s.cfg.ConsulCompat {
		zones = append(zones, consulDomain)
	}
	for _, zone := range zones {
		fqdn := dns.Fqdn(zone)
		if name == fqdn {
			return zone, nil, true
		}
		if strings.HasSuffix(name, "."+fqdn) {
			return zone, dns.SplitDomainName(strings.TrimSuffix(name, "."+fqdn)), true
		}
	}
	return "", nil, false
}

// serviceLookup is a parsed service name.
type serviceLookup struct {
	query   discovery.Query
	refused bool
}

// parseService parses the labels of a service name of zone.
func (s *Server) parseService(zone string, labels []string) (*serviceLookup, error) {
	idx := -1
	for i, l := range labels {
		if l == "service" {
			idx = i
			break
		}
	}
	if idx < 1 || idx > 2 {
		return nil, errors.New("not a service name")
	}

	lookup := &serviceLookup{query: discovery.Query{Namespace: s.cfg.Namespaces[0]}}
	front, rest := labels[:idx], labels[idx+1:]

	// RFC 2782: _service._tag or _service._tcp
	if strings.HasPrefix(front[0], "_") {
		if len(front) != 2 || !strings.HasPrefix(front[1], "_") {
			return nil, errors.New("invalid RFC 2782 name")
		}
		lookup.query.Name = strings.TrimPrefix(front[0], "_")
		if tag := strings.TrimPrefix(front[1], "_"); tag != "tcp" && tag != "udp" {
			lookup.query.Tag = tag
		}
	} else if len(front) == 2 {
		lookup.query.Tag, lookup.query.Name = front[0], front[1]
	} else {
		lookup.query.Name = front[0]
	}
	if lookup.query.Name == "" {
		return nil, errors.New("empty service name")
	}

	if zone == consulDomain {
		// <service>.service[.<datacenter>].consul
		switch len(rest) {
		case 0:
		case 1:
			if s.cfg.ConsulDatacenterIsRegion {
				lookup.query.Region = rest[0]
			} else {
				lookup.query.Datacenter = rest[0]
			}
		default:
			return nil, errors.New("too many labels")
		}
		return lookup, nil
	}

	switch len(rest) {
	case 0:
	case 2:
		lookup.query.Region = rest[1]
		fallthrough
	case 1:
		lookup.query.Namespace = rest[0]
	default:
		return nil, errors.New("too many labels")
	}

	allowed := false
	for _, ns := range s.cfg.Namespaces {
		if ns == lookup.query.Namespace {
			allowed = true
		}
	}
	lookup.refused = !allowed
	return lookup, nil
}

// answerService answers a service lookup.
func (s *Server) answerService(m *dns.Msg, req *dns.Msg, q dns.Question, zone string, lookup *serviceLookup) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()

	result, err := s.resolver.Resolve(ctx, lookup.query)
	if err != nil {
		s.logger.Debug("service lookup failed", "name", q.Name, "error", err)
		m.SetRcode(req, dns.RcodeServerFailure)
		return
	}
	if len(result.Instances) == 0 {
		m.SetRcode(req, dns.RcodeNameError)
		m.Ns = append(m.Ns, s.soa(zone))
		return
	}

	instances := make([]*structs.ServiceRegistration, len(result.Instances))
	copy(instances, result.Instances)
	rand.Shuffle(len(instances), func(i, j int) { instances[i], instances[j] = instances[j], instances[i] })

	ttl := uint32(s.cfg.TTL / time.Second)
	seen := make(map[string]bool)

	// Instances with a host name as address (external services of kind
	// alias) are answered with a CNAME record. A name with a CNAME record has
	// no other records, so there is a single one, and only if no instance
	// has an IP address.
	hasIP := false
	for _, inst := range instances {
		if net.ParseIP(inst.Address) != nil {
			hasIP = true
			break
		}
	}
	cname := false

	for _, inst := range instances {
		ip := net.ParseIP(inst.Address)

		switch q.Qtype {
		case dns.TypeA, dns.TypeAAAA, dns.TypeANY, dns.TypeCNAME:
			if ip == nil {
				if hasIP || cname {
					continue
				}
				cname = true
				m.Answer = append(m.Answer, addressRecord(q.Name, nil, inst.Address, q.Qtype, ttl))
				continue
			}
			if q.Qtype == dns.TypeCNAME || seen[inst.Address] {
				continue
			}
			seen[inst.Address] = true
			if rr := addressRecord(q.Name, ip, inst.Address, q.Qtype, ttl); rr != nil {
				m.Answer = append(m.Answer, rr)
			}

		case dns.TypeSRV:
			target := dns.Fqdn(inst.Address)
			if ip != nil {
				target = addrName(ip, zone)
			}
			m.Answer = append(m.Answer, &dns.SRV{
				Hdr:      dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: ttl},
				Priority: 1,
				Weight:   1,
				Port:     uint16(inst.Port),
				Target:   target,
			})
			if ip != nil && !seen[inst.Address] {
				seen[inst.Address] = true
				if rr := addressRecord(target, ip, inst.Address, dns.TypeANY, ttl); rr != nil {
					m.Extra = append(m.Extra, rr)
				}
			}
		}
	}

	if len(m.Answer) == 0 {
		// the service exists but has no record of the asked type
		m.Ns = append(m.Ns, s.soa(zone))
	}
}

// addressRecord returns the A, AAAA or CNAME record of an instance address
// for qtype, or nil if the address does not fit it.
func addressRecord(name string, ip net.IP, address string, qtype uint16, ttl uint32) dns.RR {
	if ip == nil {
		// a host name
		return &dns.CNAME{
			Hdr:    dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: ttl},
			Target: dns.Fqdn(address),
		}
	}
	if v4 := ip.To4(); v4 != nil {
		if qtype == dns.TypeAAAA {
			return nil
		}
		return &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: v4}
	}
	if qtype == dns.TypeA {
		return nil
	}
	return &dns.AAAA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}, AAAA: ip}
}

// addrName returns the name <hex IP>.addr.<zone> of an address.
func addrName(ip net.IP, zone string) string {
	b := ip.To4()
	if b == nil {
		b = ip.To16()
	}
	return hex.EncodeToString(b) + ".addr." + dns.Fqdn(zone)
}

// answerAddr answers <hex IP>.addr names.
func (s *Server) answerAddr(m *dns.Msg, q dns.Question, zone string, labels []string) {
	ttl := uint32(s.cfg.TTL / time.Second)
	raw, err := hex.DecodeString(labels[0])
	if err != nil || (len(raw) != net.IPv4len && len(raw) != net.IPv6len) || labels[1] != "addr" {
		m.Rcode = dns.RcodeNameError
		m.Ns = append(m.Ns, s.soa(zone))
		return
	}
	ip := net.IP(raw)
	if rr := addressRecord(q.Name, ip, ip.String(), q.Qtype, ttl); rr != nil &&
		(q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA || q.Qtype == dns.TypeANY) {
		m.Answer = append(m.Answer, rr)
		return
	}
	m.Ns = append(m.Ns, s.soa(zone))
}

// soa returns the SOA record of zone; its minimum TTL is the negative cache
// TTL of the answers.
func (s *Server) soa(zone string) dns.RR {
	ttl := uint32(s.cfg.TTL / time.Second)
	fqdn := dns.Fqdn(zone)
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: fqdn, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:      "ns." + fqdn,
		Mbox:    "hostmaster." + fqdn,
		Serial:  uint32(time.Now().Unix()),
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  ttl,
	}
}

// recurse forwards a query outside the zones of the server to the recursors,
// or refuses it.
func (s *Server) recurse(w dns.ResponseWriter, req *dns.Msg) {
	if len(s.cfg.Recursors) == 0 || !req.RecursionDesired {
		m := new(dns.Msg)
		m.SetRcode(req, dns.RcodeRefused)
		s.write(w, req, m)
		return
	}

	network := "udp"
	if _, tcp := w.RemoteAddr().(*net.TCPAddr); tcp {
		network = "tcp"
	}
	client := &dns.Client{Net: network, Timeout: recursorTimeout}

	for _, recursor := range s.cfg.Recursors {
		resp, _, err := client.Exchange(req, recursor)
		if err != nil {
			s.logger.Debug("recursor failed", "recursor", recursor, "error", err)
			continue
		}
		resp.Compress = true
		if err := w.WriteMsg(resp); err != nil {
			s.logger.Debug("failed to write dns answer", "error", err)
		}
		return
	}

	m := new(dns.Msg)
	m.SetRcode(req, dns.RcodeServerFailure)
	s.write(w, req, m)
}
