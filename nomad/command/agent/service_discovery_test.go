// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	clientconfig "github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

func TestApiServicesToStructs_Discovery(t *testing.T) {
	ci.Parallel(t)

	out := ApiServicesToStructs([]*api.Service{
		{Name: "a", Provider: "nomad", Discovery: &api.ServiceDiscovery{Prefer: "zone", MinLocal: 2, Failover: []string{"eu"}}},
		{Name: "b", Provider: "nomad", Discovery: &api.ServiceDiscovery{OverflowFraction: pointer.Of(0.0)}},
		{Name: "c", Provider: "nomad"},
	}, true)

	must.Eq(t, &structs.ServiceDiscovery{
		Prefer: "zone", MinLocal: 2, OverflowFraction: structs.DefaultDiscoveryOverflowFraction, Failover: []string{"eu"},
	}, out[0].Discovery)
	must.Eq(t, 0.0, out[1].Discovery.OverflowFraction)
	must.Nil(t, out[2].Discovery)
}

func TestHTTP_ServiceGetHealthy(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		regs := []*structs.ServiceRegistration{
			{ID: "ok", ServiceName: "web", Namespace: "default", NodeID: "n", JobID: "j", AllocID: "a", Address: "10.0.0.1",
				Ready: true, Health: structs.ServiceHealthPassing},
			{ID: "bad", ServiceName: "web", Namespace: "default", NodeID: "n", JobID: "j", AllocID: "b", Address: "10.0.0.2",
				Ready: true, Health: structs.ServiceHealthCritical},
		}
		must.NoError(t, s.server.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 10, regs))

		get := func(srv *HTTPServer, query string) []*structs.ServiceRegistration {
			req, err := http.NewRequest(http.MethodGet, "/v1/service/web"+query, nil)
			must.NoError(t, err)
			obj, err := srv.ServiceRegistrationRequest(httptest.NewRecorder(), req)
			must.NoError(t, err)
			return obj.([]*structs.ServiceRegistration)
		}

		must.Len(t, 2, get(s.Server, ""))
		must.Len(t, 1, get(s.Server, "?healthy=true"))
		must.Len(t, 2, get(s.Server, "?healthy=false"))

		req, _ := http.NewRequest(http.MethodGet, "/v1/service/web?healthy=maybe", nil)
		_, err := s.Server.ServiceRegistrationRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "invalid healthy value")

		// lookups made by workloads follow default_healthy_only
		builtin := &HTTPServer{agent: s.Agent, Addr: builtinHTTPAddr, logger: s.Server.logger}
		must.Len(t, 2, get(builtin, ""))
		s.Agent.Client().UpdateConfig(func(c *clientconfig.Config) {
			c.NativeServiceDiscovery = &config.NativeServiceDiscoveryConfig{DefaultHealthyOnly: pointer.Of(true)}
		})
		must.Len(t, 1, get(builtin, ""))
		must.Len(t, 2, get(builtin, "?healthy=false"))
	})
}
