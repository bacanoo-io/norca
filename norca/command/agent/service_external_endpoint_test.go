// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestHTTP_ExternalServices exercises the native external service endpoints
// with the api client.
func TestHTTP_ExternalServices(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()
		services := client.Services()

		// a TTL service: passing at registration
		id, _, err := services.RegisterExternal(&api.ExternalService{
			Name: "billing-db", Address: "10.0.0.5", Port: 5432, Tags: []string{"primary"},
			Checks: []*api.ExternalServiceCheck{{Type: api.ExternalCheckTypeTTL, TTL: time.Minute}},
		}, nil)
		must.NoError(t, err)
		must.Eq(t, "billing-db", id)

		regs, _, err := services.Get("billing-db", nil)
		must.NoError(t, err)
		must.Len(t, 1, regs)
		must.NotNil(t, regs[0].External)
		must.Eq(t, api.ExternalServiceKindAddress, regs[0].External.Kind)
		must.Eq(t, api.ServiceHealthPassing, regs[0].Health)
		must.Eq(t, "ttl", regs[0].Checks[0].ID)
		must.Eq(t, structs.DefaultExternalDeregisterCriticalAfter, regs[0].External.DeregisterCriticalAfter)

		// TTL updates
		_, err = services.UpdateExternalTTL("billing-db", &api.ExternalCheckUpdate{
			Status: api.ServiceHealthCritical, Output: "maintenance"}, nil)
		must.NoError(t, err)
		regs, _, err = services.Get("billing-db", nil)
		must.NoError(t, err)
		must.Eq(t, api.ServiceHealthCritical, regs[0].Health)
		must.Eq(t, "maintenance", regs[0].Checks[0].Output)
		_, err = services.UpdateExternalTTL("billing-db", nil, nil)
		must.NoError(t, err)

		// the query parameter form
		req, err := http.NewRequest(http.MethodPut, "/v1/service/external/billing-db/ttl?status=warning", nil)
		must.NoError(t, err)
		respW := httptest.NewRecorder()
		_, err = s.Server.ServiceRegistrationRequest(respW, req)
		must.NoError(t, err)
		regs, _, err = services.Get("billing-db", nil)
		must.NoError(t, err)
		must.Eq(t, api.ServiceHealthWarning, regs[0].Health)

		// a DNS alias
		_, _, err = services.RegisterExternal(&api.ExternalService{
			Name: "reports-db", Target: "reports.example.com",
		}, nil)
		must.NoError(t, err)
		regs, _, err = services.Get("reports-db", nil)
		must.NoError(t, err)
		must.Eq(t, api.ExternalServiceKindAlias, regs[0].External.Kind)
		must.Eq(t, "reports.example.com", regs[0].Address)

		list, _, err := services.ListExternal(nil)
		must.NoError(t, err)
		must.Len(t, 2, list)

		// invalid registrations
		_, _, err = services.RegisterExternal(&api.ExternalService{Name: "bad", Address: "10.0.0.1",
			Checks: []*api.ExternalServiceCheck{{Type: "script"}}}, nil)
		must.ErrorContains(t, err, "unsupported check type")

		// http checks run on the client agent the service was registered
		// through
		if s.Agent.Client() != nil {
			must.Wait(t, wait.InitialSuccess(
				wait.BoolFunc(func() bool {
					node, _ := s.Agent.server.State().NodeByID(nil, s.Agent.Client().NodeID())
					return node != nil && node.Status == structs.NodeStatusReady
				}),
				wait.Timeout(20*time.Second), wait.Gap(50*time.Millisecond),
			))
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(backend.Close)

			_, _, err = services.RegisterExternal(&api.ExternalService{
				Name: "web", Address: "127.0.0.1", Port: 80,
				Checks: []*api.ExternalServiceCheck{{Type: api.ExternalCheckTypeHTTP, HTTP: backend.URL,
					Interval: time.Second, InitialStatus: api.ServiceHealthCritical}},
			}, nil)
			must.NoError(t, err)
			must.Wait(t, wait.InitialSuccess(
				wait.BoolFunc(func() bool {
					regs, _, err := services.Get("web", nil)
					return err == nil && len(regs) == 1 && regs[0].Health == api.ServiceHealthPassing
				}),
				wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond),
			))
			regs, _, err = services.Get("web", nil)
			must.NoError(t, err)
			must.Eq(t, s.Agent.Client().NodeID(), regs[0].NodeID)
		}

		// deregistration
		_, err = services.DeregisterExternal("billing-db", nil)
		must.NoError(t, err)
		regs, _, err = services.Get("billing-db", nil)
		must.NoError(t, err)
		must.Len(t, 0, regs)
		_, err = services.DeregisterExternal("billing-db", nil)
		must.ErrorContains(t, err, "not found")
	})
}

// TestHTTP_ExternalServices_ParseFormat exercises the conversion between the
// HCL form and the API form of external services.
func TestHTTP_ExternalServices_ParseFormat(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		services := s.Client().Services()

		const text = `
external_service "orders-db" {
  namespace = "prod"
  address   = "10.20.0.5"
  port      = 5432

  check {
    type = "tcp"
    tcp  = "10.20.0.5:5432"
  }

  checked_by {
    node_pool = "edge"
  }
}
`
		parsed, err := services.ParseExternal(&api.ExternalServiceParseRequest{Text: text}, nil)
		must.NoError(t, err)
		must.Len(t, 0, parsed.Problems)
		must.Len(t, 1, parsed.Items)
		must.Eq(t, "prod", parsed.Items[0].Namespace)
		must.True(t, parsed.Items[0].Service.Declared)
		must.Eq(t, "edge", parsed.Items[0].Service.CheckedBy.NodePool)

		// the formatted text parses back to the same item
		formatted, err := services.FormatExternal(parsed.Items, nil)
		must.NoError(t, err)
		again, err := services.ParseExternal(&api.ExternalServiceParseRequest{Text: formatted}, nil)
		must.NoError(t, err)
		must.Eq(t, parsed.Items, again.Items)

		// problems are returned with their line, not as an error
		parsed, err = services.ParseExternal(&api.ExternalServiceParseRequest{
			Text: `external_service "a" {
  address = "x"
  nope = 1
}
`}, nil)
		must.NoError(t, err)
		must.Len(t, 0, parsed.Items)
		must.Len(t, 1, parsed.Problems)
		must.Eq(t, 3, parsed.Problems[0].Line)

		// semantic errors name the service
		parsed, err = services.ParseExternal(&api.ExternalServiceParseRequest{
			Text: `external_service "a" {
  port = 70000
}
`}, nil)
		must.NoError(t, err)
		must.Len(t, 1, parsed.Problems)
		must.StrContains(t, parsed.Problems[0].Message, `"a"`)

		// the JSON form
		parsed, err = services.ParseExternal(&api.ExternalServiceParseRequest{
			Text: `{"Name":"cache","Address":"10.0.0.9"}`, JSON: true}, nil)
		must.NoError(t, err)
		must.Len(t, 1, parsed.Items)

		// a declared service with node checks needs checked_by
		_, _, err = services.RegisterExternal(&api.ExternalService{
			Name: "x", Address: "10.0.0.1", Declared: true,
			Checks: []*api.ExternalServiceCheck{{Type: api.ExternalCheckTypeTCP, TCP: "10.0.0.1:80"}},
		}, nil)
		must.ErrorContains(t, err, "need checked_by")

		// a service can still be named "parse": only POST converts
		_, _, err = services.RegisterExternal(&api.ExternalService{Name: "parse", Address: "10.0.0.1"}, nil)
		must.NoError(t, err)
	})
}
