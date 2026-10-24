// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat_test

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	ctdep "github.com/hashicorp/consul-template/dependency"
	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// cluster is a Nomad server and a client serving the Consul-compatible API.
type cluster struct {
	server *nomad.Server
	client *client.Client
	consul *consulapi.Client
	addr   string
	index  uint64
	root   *structs.ACLToken
}

func newCluster(t *testing.T, acl bool, compat *structsc.ConsulCompatConfig) *cluster {
	t.Helper()
	c := &cluster{index: 1000}

	var cleanupS func()
	if acl {
		c.server, c.root, cleanupS = nomad.TestACLServer(t, nil)
	} else {
		c.server, cleanupS = nomad.TestServer(t, nil)
	}
	t.Cleanup(cleanupS)
	testutil.WaitForLeader(t, c.server.RPC)

	if compat == nil {
		compat = &structsc.ConsulCompatConfig{}
	}
	compat.Enabled = pointer.Of(true)
	compat.Bind = "127.0.0.1:0"

	var cleanupC func() error
	c.client, cleanupC = client.TestClient(t, func(cfg *config.Config) {
		cfg.Servers = []string{c.server.GetConfig().RPCAddr.String()}
		cfg.ConsulCompat = compat
		cfg.Node.Meta["zone"] = "fsn1"
		cfg.Node.Meta["rack"] = "r1"
		cfg.ACLEnabled = acl
	})
	t.Cleanup(func() { _ = cleanupC() })

	// the node must be registered before it can read the catalog
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			node, _ := c.server.State().NodeByID(nil, c.client.NodeID())
			return node != nil && node.Status == structs.NodeStatusReady
		}),
		wait.Timeout(20*time.Second), wait.Gap(50*time.Millisecond),
	))

	c.addr = c.client.ConsulCompatAddr()
	must.NotEq(t, "", c.addr)
	var err error
	c.consul, err = consulapi.NewClient(&consulapi.Config{Address: c.addr})
	must.NoError(t, err)
	return c
}

// register writes registrations on the node of the client.
func (c *cluster) register(t *testing.T, regs ...*structs.ServiceRegistration) {
	t.Helper()
	for _, reg := range regs {
		reg.NodeID = c.client.NodeID()
		if reg.JobID == "" {
			reg.JobID = "job"
		}
		if reg.Namespace == "" {
			reg.Namespace = "default"
		}
		if reg.Datacenter == "" {
			reg.Datacenter = "dc1"
		}
	}
	c.index++
	must.NoError(t, c.server.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, c.index, regs))
}

func web(id, address string, healthy bool, tags ...string) *structs.ServiceRegistration {
	reg := &structs.ServiceRegistration{
		ID: id, ServiceName: "web", AllocID: id, Address: address, Port: 8080, Tags: tags,
		Ready: true, Health: structs.ServiceHealthPassing, Meta: map[string]string{"version": "2"},
		Checks: []*structs.ServiceRegistrationCheck{{Name: "http", Role: "readiness", Status: structs.ServiceHealthPassing, Output: "200 OK"}},
	}
	if !healthy {
		reg.Health = structs.ServiceHealthCritical
		reg.Ready = false
		reg.Checks[0].Status = structs.ServiceHealthCritical
		reg.Checks[0].Output = "connection refused"
	}
	return reg
}

func serviceIDs(entries []*consulapi.ServiceEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Service.ID)
	}
	sort.Strings(out)
	return out
}

// TestConsulCompat_Conformance runs the official Consul API client against
// the supported endpoints, with the answers a Consul agent gives for the same
// catalog.
func TestConsulCompat_Conformance(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil)
	c.register(t,
		web("web-1", "10.0.0.1", true, "blue", "http"),
		web("web-2", "10.0.0.2", true, "green"),
		web("web-3", "10.0.0.3", false, "blue"),
		&structs.ServiceRegistration{ID: "db-1", ServiceName: "db", AllocID: "db-1", Address: "10.0.1.1", Port: 5432,
			Ready: true}, // written by a client that predates the health state
	)
	node := c.client.Node()

	// agent/self: the datacenter is the region
	self, err := c.consul.Agent().Self()
	must.NoError(t, err)
	must.Eq[any](t, "global", self["Config"]["Datacenter"])
	must.Eq[any](t, node.Name, self["Config"]["NodeName"])
	name, err := c.consul.Agent().NodeName()
	must.NoError(t, err)
	must.Eq(t, node.Name, name)

	// status
	leader, err := c.consul.Status().Leader()
	must.NoError(t, err)
	must.NotEq(t, "", leader)
	peers, err := c.consul.Status().Peers()
	must.NoError(t, err)
	must.Len(t, 1, peers)

	// catalog
	dcs, err := c.consul.Catalog().Datacenters()
	must.NoError(t, err)
	must.Eq(t, []string{"global"}, dcs)

	services, meta, err := c.consul.Catalog().Services(nil)
	must.NoError(t, err)
	must.Eq(t, map[string][]string{"web": {"blue", "green", "http"}, "db": {}}, services)
	must.Positive(t, meta.LastIndex)
	must.True(t, meta.KnownLeader)

	// node meta filtering
	services, _, err = c.consul.Catalog().Services(&consulapi.QueryOptions{NodeMeta: map[string]string{"zone": "fsn1"}})
	must.NoError(t, err)
	must.MapLen(t, 2, services)
	services, _, err = c.consul.Catalog().Services(&consulapi.QueryOptions{NodeMeta: map[string]string{"zone": "nbg1"}})
	must.NoError(t, err)
	must.MapEmpty(t, services)

	catalog, _, err := c.consul.Catalog().Service("web", "blue", nil)
	must.NoError(t, err)
	must.Len(t, 2, catalog)
	for _, svc := range catalog {
		must.Eq(t, node.Name, svc.Node)
		must.Eq(t, node.ID, svc.ID)
		must.Eq(t, "global", svc.Datacenter)
		must.Eq(t, 8080, svc.ServicePort)
		must.Eq(t, "2", svc.ServiceMeta["version"])
		must.Eq(t, "nomad", svc.ServiceMeta["external-source"])
		must.Eq(t, "fsn1", svc.NodeMeta["zone"])
	}

	nodes, _, err := c.consul.Catalog().Nodes(nil)
	must.NoError(t, err)
	must.Len(t, 1, nodes)
	must.Eq(t, node.Name, nodes[0].Node)
	must.Eq(t, "r1", nodes[0].Meta["rack"])

	catalogNode, _, err := c.consul.Catalog().Node(node.Name, nil)
	must.NoError(t, err)
	must.MapLen(t, 4, catalogNode.Services)
	must.Eq(t, "web", catalogNode.Services["web-1"].Service)

	// health: passing filter, aggregated status, checks
	entries, _, err := c.consul.Health().Service("web", "", false, nil)
	must.NoError(t, err)
	must.Eq(t, []string{"web-1", "web-2", "web-3"}, serviceIDs(entries))

	entries, _, err = c.consul.Health().Service("web", "", true, nil)
	must.NoError(t, err)
	must.Eq(t, []string{"web-1", "web-2"}, serviceIDs(entries))
	for _, e := range entries {
		must.Eq(t, consulapi.HealthPassing, e.Checks.AggregatedStatus())
		must.Eq(t, "serfHealth", e.Checks[0].CheckID)
		must.Eq(t, node.Name, e.Node.Node)
	}

	entries, _, err = c.consul.Health().ServiceMultipleTags("web", []string{"blue"}, false, nil)
	must.NoError(t, err)
	must.Eq(t, []string{"web-1", "web-3"}, serviceIDs(entries))

	entries, _, err = c.consul.Health().Service("web", "", false,
		&consulapi.QueryOptions{Filter: `"green" in Service.Tags`})
	must.NoError(t, err)
	must.Eq(t, []string{"web-2"}, serviceIDs(entries))

	_, _, err = c.consul.Health().Service("web", "", false, &consulapi.QueryOptions{Filter: `Service.Tags ==`})
	must.ErrorContains(t, err, "400")

	// registrations without health are healthy while ready
	entries, _, err = c.consul.Health().Service("db", "", true, nil)
	must.NoError(t, err)
	must.Len(t, 1, entries)

	checks, _, err := c.consul.Health().Checks("web", nil)
	must.NoError(t, err)
	must.Len(t, 6, checks)

	critical, _, err := c.consul.Health().State(consulapi.HealthCritical, nil)
	must.NoError(t, err)
	must.Len(t, 2, critical, must.Sprint("the aggregate and the readiness check of web-3"))
	for _, check := range critical {
		must.Eq(t, "web-3", check.ServiceID)
	}

	connect, _, err := c.consul.Health().Connect("web", "", true, nil)
	must.NoError(t, err)
	must.Len(t, 0, connect)

	// unknown services
	entries, _, err = c.consul.Health().Service("nope", "", true, nil)
	must.NoError(t, err)
	must.Len(t, 0, entries)

	// unsupported endpoints answer 501
	_, _, err = c.consul.KV().Get("key", nil)
	must.ErrorContains(t, err, "501")
	err = c.consul.Agent().ServiceRegister(&consulapi.AgentServiceRegistration{Name: "x"})
	must.ErrorContains(t, err, "501")
	_, _, err = c.consul.Catalog().GatewayServices("x", nil)
	must.ErrorContains(t, err, "501")
	resp, err := http.Get("http://" + c.addr + "/v1/session/list")
	must.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	must.Eq(t, http.StatusNotImplemented, resp.StatusCode)
	must.StrContains(t, string(body), "not supported")
}

// TestConsulCompat_Blocking checks blocking queries: they wait for a change
// of the catalog and return its new index.
func TestConsulCompat_Blocking(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil)
	c.register(t, web("web-1", "10.0.0.1", true), web("web-2", "10.0.0.2", true))

	_, meta, err := c.consul.Health().Service("web", "", true, nil)
	must.NoError(t, err)

	type result struct {
		entries []*consulapi.ServiceEntry
		meta    *consulapi.QueryMeta
		err     error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		entries, meta, err := c.consul.Health().Service("web", "", true,
			&consulapi.QueryOptions{WaitIndex: meta.LastIndex, WaitTime: 10 * time.Second})
		done <- result{entries, meta, err}
	}()

	time.Sleep(300 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("blocking query returned without a change")
	default:
	}

	// web-2 fails: it leaves the passing answer
	c.register(t, web("web-2", "10.0.0.2", false))
	select {
	case res := <-done:
		must.NoError(t, res.err)
		must.Greater(t, meta.LastIndex, res.meta.LastIndex)
		must.Eq(t, []string{"web-1"}, serviceIDs(res.entries))
		must.Less(t, 5*time.Second, time.Since(start))
	case <-time.After(10 * time.Second):
		t.Fatal("blocking query did not return")
	}

	// a blocking query without change returns after its wait time
	_, meta, err = c.consul.Catalog().Services(nil)
	must.NoError(t, err)
	start = time.Now()
	_, meta2, err := c.consul.Catalog().Services(&consulapi.QueryOptions{WaitIndex: meta.LastIndex, WaitTime: time.Second})
	must.NoError(t, err)
	must.Eq(t, meta.LastIndex, meta2.LastIndex)
	must.Greater(t, 900*time.Millisecond, time.Since(start))
}

// TestConsulCompat_ACL checks that requests use the Nomad ACL token they
// carry, or the anonymous token.
func TestConsulCompat_ACL(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, true, nil)
	c.register(t, web("web-1", "10.0.0.1", true))

	// no token, and the anonymous policy of Nomad allows nothing
	_, _, err := c.consul.Health().Service("web", "", true, nil)
	must.ErrorContains(t, err, "403")

	reader := mock.CreatePolicyAndToken(t, c.server.State(), 2000, "read-default",
		`namespace "default" { policy = "read" }`)

	entries, _, err := c.consul.Health().Service("web", "", true, &consulapi.QueryOptions{Token: reader.SecretID})
	must.NoError(t, err)
	must.Len(t, 1, entries)

	other := mock.CreatePolicyAndToken(t, c.server.State(), 2001, "read-other",
		`namespace "other" { policy = "read" }`)
	_, _, err = c.consul.Health().Service("web", "", true, &consulapi.QueryOptions{Token: other.SecretID})
	must.ErrorContains(t, err, "403")

	// the token may come as a query parameter too
	resp, err := http.Get(fmt.Sprintf("http://%s/v1/catalog/services?token=%s", c.addr, reader.SecretID))
	must.NoError(t, err)
	resp.Body.Close()
	must.Eq(t, http.StatusOK, resp.StatusCode)

	// the anonymous token serves requests without a token
	secret := uuid.Generate()
	anon := newCluster(t, true, &structsc.ConsulCompatConfig{AnonymousToken: secret})
	mock.CreatePolicy(t, anon.server.State(), 2000, "read-default", `namespace "default" { policy = "read" }`)
	token := mock.ACLToken()
	token.SecretID = secret
	token.Policies = []string{"read-default"}
	must.NoError(t, anon.server.State().UpsertACLTokens(structs.MsgTypeTestSetup, 2001, []*structs.ACLToken{token}))
	anon.register(t, web("web-1", "10.0.0.1", true))

	entries, _, err = anon.consul.Health().Service("web", "", true, nil)
	must.NoError(t, err)
	must.Len(t, 1, entries)

	// a token in the request wins over the anonymous token
	_, _, err = anon.consul.Health().Service("web", "", true, &consulapi.QueryOptions{Token: uuid.Generate()})
	must.ErrorContains(t, err, "403")
}

// TestConsulCompat_Namespaces checks the names of services of several
// namespaces, and Consul datacenters mapped to Nomad datacenters.
func TestConsulCompat_Namespaces(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, &structsc.ConsulCompatConfig{
		Namespaces:     []string{"default", "prod"},
		DatacenterFrom: structsc.ConsulCompatDatacenterFromDatacenter,
	})
	must.NoError(t, c.server.State().UpsertNamespaces(900, []*structs.Namespace{{Name: "prod"}}))

	prod := web("prod-1", "10.0.0.9", true)
	prod.Namespace = "prod"
	other := web("web-dc2", "10.0.0.5", true)
	other.Datacenter = "dc2"
	c.register(t, web("web-1", "10.0.0.1", true), prod, other)

	services, _, err := c.consul.Catalog().Services(nil)
	must.NoError(t, err)
	must.MapContainsKeys(t, services, []string{"web", "prod--web"})

	entries, _, err := c.consul.Health().Service("prod--web", "", true, nil)
	must.NoError(t, err)
	must.Eq(t, []string{"prod-1"}, serviceIDs(entries))
	must.Eq(t, "prod--web", entries[0].Service.Service)

	// datacenters are the Nomad datacenters
	self, err := c.consul.Agent().Self()
	must.NoError(t, err)
	must.Eq[any](t, "dc1", self["Config"]["Datacenter"])

	entries, _, err = c.consul.Health().Service("web", "", true, &consulapi.QueryOptions{Datacenter: "dc2"})
	must.NoError(t, err)
	must.Eq(t, []string{"web-dc2"}, serviceIDs(entries))
	must.Eq(t, "dc2", entries[0].Node.Datacenter)
}

// TestConsulCompat_ConsulTemplate renders the service functions of
// consul-template (service, health.service, services) against the API.
func TestConsulCompat_ConsulTemplate(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil)
	c.register(t, web("web-1", "10.0.0.1", true, "blue"), web("web-2", "10.0.0.2", false, "blue"))

	clients := ctdep.NewClientSet()
	must.NoError(t, clients.CreateConsulClient(&ctdep.CreateConsulClientInput{Address: c.addr}))
	defer clients.Stop()

	// {{ service "web" }}: healthy instances only
	q, err := ctdep.NewHealthServiceQuery("web")
	must.NoError(t, err)
	data, rm, err := q.Fetch(clients, &ctdep.QueryOptions{})
	must.NoError(t, err)
	services := data.([]*ctdep.HealthService)
	must.Len(t, 1, services)
	must.Eq(t, "10.0.0.1", services[0].Address)
	must.Eq(t, 8080, services[0].Port)
	must.Eq(t, "passing", services[0].Status)
	must.Positive(t, rm.LastIndex)

	// {{ service "blue.web|any" }}
	q, err = ctdep.NewHealthServiceQuery("blue.web|any")
	must.NoError(t, err)
	data, _, err = q.Fetch(clients, &ctdep.QueryOptions{})
	must.NoError(t, err)
	must.Len(t, 2, data.([]*ctdep.HealthService))

	// {{ services }}
	sq, err := ctdep.NewCatalogServicesQuery("")
	must.NoError(t, err)
	data, _, err = sq.Fetch(clients, &ctdep.QueryOptions{})
	must.NoError(t, err)
	catalog := data.([]*ctdep.CatalogSnippet)
	must.Len(t, 1, catalog)
	must.Eq(t, "web", catalog[0].Name)

	// a blocking fetch sees web-2 recover
	done := make(chan []*ctdep.HealthService, 1)
	go func() {
		q, _ := ctdep.NewHealthServiceQuery("web")
		data, _, err := q.Fetch(clients, &ctdep.QueryOptions{WaitIndex: rm.LastIndex, WaitTime: 10 * time.Second})
		if err == nil {
			done <- data.([]*ctdep.HealthService)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	c.register(t, web("web-2", "10.0.0.2", true, "blue"))
	select {
	case got := <-done:
		must.Len(t, 2, got)
	case <-time.After(10 * time.Second):
		t.Fatal("consul-template did not see the change")
	}
}

// TestConsulCompat_ToolRequestPatterns emulates the requests of Traefik's
// consulcatalog provider and Prometheus' consul_sd_configs, and checks that
// the appearance, health change and disappearance of an instance show up as
// they do with a Consul agent.
func TestConsulCompat_ToolRequestPatterns(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil)
	c.register(t, web("web-1", "10.0.0.1", true, "traefik.enable=true"))

	// Traefik: blocking Catalog().Services, then Health().Service(passing)
	// per service, and Health().Connect in Connect mode
	traefik := func() map[string][]string {
		opts := &consulapi.QueryOptions{AllowStale: true}
		services, _, err := c.consul.Catalog().Services(opts)
		must.NoError(t, err)
		out := make(map[string][]string)
		for name := range services {
			entries, _, err := c.consul.Health().Service(name, "", true, opts)
			must.NoError(t, err)
			for _, e := range entries {
				out[name] = append(out[name], fmt.Sprintf("%s:%d", e.Service.Address, e.Service.Port))
			}
			connect, _, err := c.consul.Health().Connect(name, "", true, opts)
			must.NoError(t, err)
			must.Len(t, 0, connect)
		}
		return out
	}

	// Prometheus: Agent().Self() for the datacenter, blocking
	// Catalog().Services with node meta, then
	// Health().ServiceMultipleTags(passingOnly=false) per service; the
	// health of a target is the aggregated status of its checks
	prometheus := func() map[string]string {
		self, err := c.consul.Agent().Self()
		must.NoError(t, err)
		dc := self["Config"]["Datacenter"].(string)
		opts := &consulapi.QueryOptions{AllowStale: true, Datacenter: dc, NodeMeta: map[string]string{"zone": "fsn1"}}
		services, _, err := c.consul.Catalog().Services(opts)
		must.NoError(t, err)
		targets := make(map[string]string)
		for name := range services {
			entries, _, err := c.consul.Health().ServiceMultipleTags(name, nil, false, opts)
			must.NoError(t, err)
			for _, e := range entries {
				addr := e.Service.Address
				if addr == "" {
					addr = e.Node.Address
				}
				targets[fmt.Sprintf("%s:%d", addr, e.Service.Port)] = e.Checks.AggregatedStatus()
				must.Eq(t, dc, e.Node.Datacenter)
			}
		}
		return targets
	}

	must.Eq(t, map[string][]string{"web": {"10.0.0.1:8080"}}, traefik())
	must.Eq(t, map[string]string{"10.0.0.1:8080": "passing"}, prometheus())

	// appearance
	c.register(t, web("web-2", "10.0.0.2", true))
	must.Eq(t, map[string][]string{"web": {"10.0.0.1:8080", "10.0.0.2:8080"}}, sorted(traefik()))
	must.MapLen(t, 2, prometheus())

	// health change
	c.register(t, web("web-2", "10.0.0.2", false))
	must.Eq(t, map[string][]string{"web": {"10.0.0.1:8080"}}, traefik())
	must.Eq(t, "critical", prometheus()["10.0.0.2:8080"])

	// disappearance
	must.NoError(t, c.server.State().DeleteServiceRegistrationByID(structs.MsgTypeTestSetup, 3000, "default", "web-2"))
	must.MapLen(t, 1, prometheus())
}

func sorted(m map[string][]string) map[string][]string {
	for _, v := range m {
		sort.Strings(v)
	}
	return m
}
