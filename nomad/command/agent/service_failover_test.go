// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// regionRPCer answers service lookups per region.
type regionRPCer struct {
	regions map[string][]*structs.ServiceRegistration
	asked   []string
}

func (r *regionRPCer) RPC(method string, args, reply any) error {
	req := args.(*structs.ServiceRegistrationByNameRequest)
	r.asked = append(r.asked, req.Region)
	regs, ok := r.regions[req.Region]
	if !ok {
		return errors.New("No path to region")
	}
	resp := reply.(*structs.ServiceRegistrationByNameResponse)
	resp.Services = regs
	resp.Index = 42
	return nil
}
func (r *regionRPCer) Server() *nomad.Server               { return nil }
func (r *regionRPCer) Client() *client.Client              { return nil }
func (r *regionRPCer) Stats() map[string]map[string]string { return nil }
func (r *regionRPCer) GetConfig() *Config                  { return DefaultConfig() }
func (r *regionRPCer) GetMetricsSink() *metrics.InmemSink  { return nil }

func TestHTTP_ServiceFailover(t *testing.T) {
	ci.Parallel(t)

	rpcer := &regionRPCer{regions: map[string][]*structs.ServiceRegistration{
		"us": nil,
		"ap": {{ID: "ap1"}},
	}}
	s := &HTTPServer{agent: rpcer, logger: testlog.HCLogger(t)}

	near := &structs.Topology{Region: "eu", Datacenter: "dc1"}
	args := &structs.ServiceRegistrationByNameRequest{
		ServiceName:  "api",
		Near:         near,
		QueryOptions: structs.QueryOptions{Region: "eu", MinQueryIndex: 7},
	}
	reply := &structs.ServiceRegistrationByNameResponse{
		Discovery: &structs.ServiceDiscovery{Failover: []string{"gone", "eu", "us", "ap"}},
		QueryMeta: structs.QueryMeta{Index: 9},
	}

	s.serviceFailover(args, reply)
	must.Len(t, 1, reply.Services)
	must.Eq(t, "ap1", reply.Services[0].ID)
	must.Eq(t, uint64(9), reply.Index, must.Sprint("the local index is kept"))
	must.Eq(t, []string{"gone", "us", "ap"}, rpcer.asked)

	// no failover without near, with instances, or for another region
	for _, tc := range []func(*structs.ServiceRegistrationByNameRequest, *structs.ServiceRegistrationByNameResponse){
		func(a *structs.ServiceRegistrationByNameRequest, _ *structs.ServiceRegistrationByNameResponse) {
			a.Near = nil
		},
		func(_ *structs.ServiceRegistrationByNameRequest, r *structs.ServiceRegistrationByNameResponse) {
			r.Services = []*structs.ServiceRegistration{{ID: "local"}}
		},
		func(a *structs.ServiceRegistrationByNameRequest, _ *structs.ServiceRegistrationByNameResponse) {
			a.Region = "us"
		},
		func(_ *structs.ServiceRegistrationByNameRequest, r *structs.ServiceRegistrationByNameResponse) {
			r.Discovery = nil
		},
	} {
		rpcer.asked = nil
		a := *args
		r := &structs.ServiceRegistrationByNameResponse{Discovery: reply.Discovery}
		tc(&a, r)
		s.serviceFailover(&a, r)
		must.Len(t, 0, rpcer.asked)
	}

	// near=self needs a client agent
	req := httptest.NewRequest(http.MethodGet, "/v1/service/api?near=self", nil)
	_, err := s.parseNear(req)
	must.ErrorContains(t, err, "requires a client agent")
	req = httptest.NewRequest(http.MethodGet, "/v1/service/api?near=rack", nil)
	_, err = s.parseNear(req)
	must.ErrorContains(t, err, `near must be "self"`)
	req = httptest.NewRequest(http.MethodGet, "/v1/service/api", nil)
	n, err := s.parseNear(req)
	must.NoError(t, err)
	must.Nil(t, n)
}

func TestHTTP_ServiceNearSelf(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, func(c *Config) {
		c.Client.Meta = map[string]string{"zone": "fsn1"}
	}, func(s *TestAgent) {
		settings := &structs.ServiceDiscovery{Prefer: "zone", MinLocal: 1}
		dc := s.Agent.Client().Datacenter()
		regs := []*structs.ServiceRegistration{
			{ID: "local", ServiceName: "api", Namespace: "default", NodeID: "n", JobID: "j", AllocID: "a",
				Address: "10.0.0.1", Datacenter: dc, Zone: "fsn1", Ready: true, Discovery: settings},
			{ID: "remote", ServiceName: "api", Namespace: "default", NodeID: "n", JobID: "j", AllocID: "b",
				Address: "10.0.0.2", Datacenter: dc, Zone: "nbg1", Ready: true, Discovery: settings},
		}
		must.NoError(t, s.server.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, regs))

		get := func(srv *HTTPServer, query string) []*structs.ServiceRegistration {
			req, err := http.NewRequest(http.MethodGet, "/v1/service/api"+query, nil)
			must.NoError(t, err)
			obj, err := srv.ServiceRegistrationRequest(httptest.NewRecorder(), req)
			must.NoError(t, err)
			return obj.([]*structs.ServiceRegistration)
		}

		must.Len(t, 2, get(s.Server, ""))
		local := get(s.Server, "?near=self")
		must.Len(t, 1, local)
		must.Eq(t, "local", local[0].ID)

		// lookups made by workloads are topology-aware by default
		builtin := &HTTPServer{agent: s.Agent, Addr: builtinHTTPAddr, logger: s.Server.logger}
		must.Len(t, 1, get(builtin, ""))
		must.Len(t, 2, get(builtin, "?near=none"))
	})
}
