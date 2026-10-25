// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat_test

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/miekg/dns"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// eventually waits until f returns true.
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(f),
		wait.Timeout(15*time.Second), wait.Gap(100*time.Millisecond),
	))
}

// withDNS enables the built-in DNS server of the client.
func withDNS(cfg *config.Config) {
	cfg.DNS = &structsc.DNSConfig{Enabled: pointer.Of(true), Bind: "127.0.0.1:0", TTL: "1s"}
}

// lookup queries the DNS server of the client and returns the answers as
// strings.
func lookup(t *testing.T, c *cluster, name string, qtype uint16) []string {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	resp, err := dns.Exchange(m, c.client.DNSAddr())
	must.NoError(t, err)
	var out []string
	for _, rr := range resp.Answer {
		switch r := rr.(type) {
		case *dns.A:
			out = append(out, r.A.String())
		case *dns.CNAME:
			out = append(out, "cname:"+r.Target)
		case *dns.SRV:
			out = append(out, fmt.Sprintf("%s:%d", r.Target, r.Port))
		}
	}
	sort.Strings(out)
	return out
}

func checkStatus(t *testing.T, c *cluster, checkID string) string {
	t.Helper()
	checks, err := c.consul.Agent().Checks()
	must.NoError(t, err)
	if check, ok := checks[checkID]; ok {
		return check.Status
	}
	return ""
}

// TestConsulCompat_AgentServices registers a service through the agent API
// of the official Consul client, as self-registering frameworks do, and
// follows it through the catalog, the health endpoints and the built-in DNS.
func TestConsulCompat_AgentServices(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil, withDNS)
	agent := c.consul.Agent()

	must.NoError(t, agent.ServiceRegister(&consulapi.AgentServiceRegistration{
		ID: "pay-1", Name: "payments", Port: 9000, Address: "10.1.0.7", Tags: []string{"v1"},
		Meta:  map[string]string{"framework": "spring"},
		Check: &consulapi.AgentServiceCheck{TTL: "3s", DeregisterCriticalServiceAfter: "1m"},
	}))

	// the agent knows the service and its check, critical until renewed
	services, err := agent.Services()
	must.NoError(t, err)
	must.MapContainsKey(t, services, "pay-1")
	must.Eq(t, "payments", services["pay-1"].Service)
	must.Eq(t, "spring", services["pay-1"].Meta["framework"])
	must.Eq(t, consulapi.HealthCritical, checkStatus(t, c, "service:pay-1"))

	entries, _, err := c.consul.Health().Service("payments", "", true, nil)
	must.NoError(t, err)
	must.Len(t, 0, entries)

	// the registration is an external service owned by the node
	reg, err := c.server.State().GetServiceRegistrationByID(nil, "default",
		"_consul-agent-"+c.client.NodeID()+"-pay-1")
	must.NoError(t, err)
	must.NotNil(t, reg)
	must.Eq(t, c.client.NodeID(), reg.NodeID)
	must.Eq(t, structs.ExternalServiceSourceConsul, reg.External.Source)
	must.Eq(t, time.Minute, reg.External.DeregisterCriticalAfter)

	// renewal
	must.NoError(t, agent.PassTTL("service:pay-1", "all good"))
	entries, _, err = c.consul.Health().Service("payments", "v1", true, nil)
	must.NoError(t, err)
	must.Len(t, 1, entries)
	must.Eq(t, "pay-1", entries[0].Service.ID)
	must.Eq(t, "10.1.0.7", entries[0].Service.Address)
	must.Eq(t, c.client.Node().Name, entries[0].Node.Node)
	var ttlCheck *consulapi.HealthCheck
	for _, check := range entries[0].Checks {
		if check.CheckID == "service:pay-1" {
			ttlCheck = check
		}
	}
	must.NotNil(t, ttlCheck)
	must.Eq(t, "ttl", ttlCheck.Type)
	must.Eq(t, "all good", ttlCheck.Output)
	must.Eq(t, "pay-1", ttlCheck.ServiceID)

	catalog, _, err := c.consul.Catalog().Service("payments", "", nil)
	must.NoError(t, err)
	must.Len(t, 1, catalog)
	must.Eq(t, 9000, catalog[0].ServicePort)

	// the built-in DNS answers it while it passes
	eventually(t, func() bool {
		must.NoError(t, agent.PassTTL("service:pay-1", "all good"))
		got := lookup(t, c, "payments.service.nomad", dns.TypeA)
		return len(got) == 1 && got[0] == "10.1.0.7"
	})
	must.NoError(t, agent.PassTTL("service:pay-1", "all good"))
	srv := lookup(t, c, "payments.service.nomad", dns.TypeSRV)
	must.Len(t, 1, srv)
	must.StrHasSuffix(t, ":9000", srv[0])

	// warning through check/update
	must.NoError(t, agent.UpdateTTL("service:pay-1", "slow", consulapi.HealthWarning))
	must.Eq(t, consulapi.HealthWarning, checkStatus(t, c, "service:pay-1"))

	// without renewal the check expires
	eventually(t, func() bool { return checkStatus(t, c, "service:pay-1") == consulapi.HealthCritical })
	checks, err := agent.Checks()
	must.NoError(t, err)
	must.Eq(t, structs.ExternalCheckTTLExpiredOutput, checks["service:pay-1"].Output)
	eventually(t, func() bool { return len(lookup(t, c, "payments.service.nomad", dns.TypeA)) == 0 })

	// errors as a Consul agent gives them
	err = agent.PassTTL("nope", "")
	must.ErrorContains(t, err, "404")
	err = agent.ServiceDeregister("nope")
	must.ErrorContains(t, err, "404")
	err = agent.ServiceRegister(&consulapi.AgentServiceRegistration{Name: "x",
		Check: &consulapi.AgentServiceCheck{Args: []string{"/bin/true"}, Interval: "10s"}})
	must.ErrorContains(t, err, "400")
	err = agent.ServiceRegister(&consulapi.AgentServiceRegistration{Name: "x",
		Connect: &consulapi.AgentServiceConnect{SidecarService: &consulapi.AgentServiceRegistration{}}})
	must.ErrorContains(t, err, "400")
	err = agent.ServiceRegister(&consulapi.AgentServiceRegistration{Name: "not_valid"})
	must.ErrorContains(t, err, "400")

	// deregistration
	must.NoError(t, agent.ServiceDeregister("pay-1"))
	services, err = agent.Services()
	must.NoError(t, err)
	must.MapLen(t, 0, services)
	catalog, _, err = c.consul.Catalog().Service("payments", "", nil)
	must.NoError(t, err)
	must.Len(t, 0, catalog)
}

// TestConsulCompat_AgentNodeChecks registers services with http and tcp
// checks, which the client agent runs.
func TestConsulCompat_AgentNodeChecks(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil)
	agent := c.consul.Agent()

	var code atomic.Int32
	code.Store(http.StatusOK)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(code.Load()))
		fmt.Fprintf(w, "status %d", code.Load())
	}))
	t.Cleanup(backend.Close)

	must.NoError(t, agent.ServiceRegister(&consulapi.AgentServiceRegistration{
		ID: "api-1", Name: "api", Port: 8080,
		Checks: consulapi.AgentServiceChecks{
			{CheckID: "api-http", HTTP: backend.URL + "/health", Interval: "1s", Timeout: "1s"},
		},
	}))
	eventually(t, func() bool { return checkStatus(t, c, "api-http") == consulapi.HealthPassing })

	// the address defaults to the address of the node
	services, err := agent.Services()
	must.NoError(t, err)
	must.NotEq(t, "", services["api-1"].Address)

	code.Store(http.StatusTooManyRequests)
	eventually(t, func() bool { return checkStatus(t, c, "api-http") == consulapi.HealthWarning })
	code.Store(http.StatusInternalServerError)
	eventually(t, func() bool { return checkStatus(t, c, "api-http") == consulapi.HealthCritical })
	checks, err := agent.Checks()
	must.NoError(t, err)
	must.StrContains(t, checks["api-http"].Output, "500")
	must.Eq(t, "http", checks["api-http"].Type)

	// a TCP check
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	must.NoError(t, agent.ServiceRegister(&consulapi.AgentServiceRegistration{
		ID: "db-1", Name: "db", Address: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Check: &consulapi.AgentServiceCheck{TCP: ln.Addr().String(), Interval: "1s"},
	}))
	eventually(t, func() bool { return checkStatus(t, c, "service:db-1") == consulapi.HealthPassing })

	// the registrations of the node go away with the node
	must.NoError(t, c.server.State().DeleteServiceRegistrationByNodeID(structs.MsgTypeTestSetup, 9000, c.client.NodeID()))
	eventually(t, func() bool {
		services, err := agent.Services()
		return err == nil && len(services) == 0
	})
}

// TestConsulCompat_ExternalAliasDNS registers DNS aliases and address
// services with the native RPC, and checks the answers of the built-in DNS
// and the catalog.
func TestConsulCompat_ExternalAliasDNS(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil, withDNS)

	upsert := func(reg *structs.ServiceRegistration) {
		t.Helper()
		args := &structs.ExternalServiceUpsertRequest{
			Service:      reg,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
		}
		var reply structs.ExternalServiceUpsertResponse
		must.NoError(t, c.server.RPC(structs.ServiceRegistrationUpsertExternalRPCMethod, args, &reply))
	}
	alias := func(target string) *structs.ServiceRegistration {
		return &structs.ServiceRegistration{
			ID: "billing-db", ServiceName: "billing-db", Address: target, Port: 5432,
			External: &structs.ServiceExternal{Kind: structs.ExternalServiceKindAlias, Source: structs.ExternalServiceSourceAPI},
		}
	}

	upsert(alias("billing.c9xk2.eu-central-1.rds.amazonaws.com"))
	must.Eq(t, []string{"cname:billing.c9xk2.eu-central-1.rds.amazonaws.com."},
		lookup(t, c, "billing-db.service.nomad", dns.TypeA))
	must.Eq(t, []string{"cname:billing.c9xk2.eu-central-1.rds.amazonaws.com."},
		lookup(t, c, "billing-db.service.nomad", dns.TypeCNAME))
	must.Eq(t, []string{"billing.c9xk2.eu-central-1.rds.amazonaws.com.:5432"},
		lookup(t, c, "billing-db.service.nomad", dns.TypeSRV))

	// a cutover changes the alias only
	upsert(alias("billing-v2.c9xk2.eu-central-1.rds.amazonaws.com"))
	eventually(t, func() bool {
		got := lookup(t, c, "billing-db.service.nomad", dns.TypeA)
		return len(got) == 1 && got[0] == "cname:billing-v2.c9xk2.eu-central-1.rds.amazonaws.com."
	})

	// the catalog shows it on the external node
	entries, _, err := c.consul.Health().Service("billing-db", "", true, nil)
	must.NoError(t, err)
	must.Len(t, 1, entries)
	must.Eq(t, "billing-v2.c9xk2.eu-central-1.rds.amazonaws.com", entries[0].Service.Address)
	must.Eq(t, "external", entries[0].Node.Node)
	must.Eq(t, "true", entries[0].Node.Meta["external-node"])

	// an address service with a TTL check answers A records
	upsert(&structs.ServiceRegistration{
		ID: "ldap", ServiceName: "ldap", Address: "192.0.2.10", Port: 389,
		External: &structs.ServiceExternal{
			Kind: structs.ExternalServiceKindAddress,
			Checks: []*structs.ExternalServiceCheck{{ID: "ttl", Type: structs.ExternalCheckTypeTTL, TTL: time.Minute,
				InitialStatus: structs.ServiceHealthPassing}},
		},
	})
	must.Eq(t, []string{"192.0.2.10"}, lookup(t, c, "ldap.service.nomad", dns.TypeA))
	must.Eq(t, []string{"c000020a.addr.nomad.:389"}, lookup(t, c, "ldap.service.nomad", dns.TypeSRV))
}

// TestConsulCompat_AgentServicesACL checks that the agent write endpoints
// need the register-external-service capability.
func TestConsulCompat_AgentServicesACL(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, true, nil)
	reg := &consulapi.AgentServiceRegistration{ID: "pay-1", Name: "payments", Port: 9000,
		Check: &consulapi.AgentServiceCheck{TTL: "30s"}}

	err := c.consul.Agent().ServiceRegister(reg)
	must.ErrorContains(t, err, "403")

	writer := mock.CreatePolicyAndToken(t, c.server.State(), 2000, "write-default",
		`namespace "default" { policy = "write" }`)
	err = c.consul.Agent().ServiceRegisterOpts(reg, consulapi.ServiceRegisterOpts{Token: writer.SecretID})
	must.ErrorContains(t, err, "403")

	registrar := mock.CreatePolicyAndToken(t, c.server.State(), 2001, "register-default",
		`namespace "default" { capabilities = ["register-external-service", "read-job"] }`)
	consul, err := consulapi.NewClient(&consulapi.Config{Address: c.addr, Token: registrar.SecretID})
	must.NoError(t, err)
	must.NoError(t, consul.Agent().ServiceRegister(reg))
	must.NoError(t, consul.Agent().PassTTL("service:pay-1", ""))

	// renewals without the capability fail
	err = c.consul.Agent().PassTTL("service:pay-1", "")
	must.ErrorContains(t, err, "403")

	entries, _, err := consul.Health().Service("payments", "", true, nil)
	must.NoError(t, err)
	must.Len(t, 1, entries)

	must.NoError(t, consul.Agent().ServiceDeregister("pay-1"))
}
