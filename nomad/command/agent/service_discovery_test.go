// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	clientconfig "github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestApiServicesToStructs_Discovery(t *testing.T) {
	ci.Parallel(t)

	out := ApiServicesToStructs([]*api.Service{
		{Name: "a", Provider: "nomad", Discovery: &api.ServiceDiscovery{Prefer: "datacenter", MinLocal: 2, Failover: []string{"eu"}}},
		{Name: "b", Provider: "nomad", Discovery: &api.ServiceDiscovery{OverflowFraction: pointer.Of(0.0)}},
		{Name: "c", Provider: "nomad"},
		{Name: "d", Provider: "nomad", Discovery: &api.ServiceDiscovery{OverflowFraction: pointer.Of(0.35)}},
	}, true)

	// an unset overflow fraction stays unset: the servers fill in their
	// default at registration
	must.Eq(t, &structs.ServiceDiscovery{
		Prefer: "datacenter", MinLocal: 2, Failover: []string{"eu"},
	}, out[0].Discovery)
	must.Nil(t, out[0].Discovery.OverflowFraction)
	must.Eq(t, pointer.Of(0.0), out[1].Discovery.OverflowFraction)
	must.Nil(t, out[2].Discovery)
	must.Eq(t, pointer.Of(0.35), out[3].Discovery.OverflowFraction)
}

// TestHTTP_JobRegister_DiscoveryDefaultOverflowFraction registers a job with
// discovery blocks over the HTTP API on an agent whose servers have a default
// overflow fraction configured: the stored job and the service registrations
// made by the client carry the server default where the job leaves the
// fraction unset, and explicit values, including 0, are kept.
func TestHTTP_JobRegister_DiscoveryDefaultOverflowFraction(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, func(c *Config) {
		c.Server.NativeServiceDiscovery = &config.ServerNativeServiceDiscoveryConfig{
			DefaultOverflowFraction: pointer.Of(0.35),
		}
	}, func(s *TestAgent) {
		client := s.Client()

		service := func(name string, discovery *api.ServiceDiscovery) *api.Service {
			return &api.Service{Name: name, PortLabel: "http", Provider: "nomad", Discovery: discovery}
		}
		job := &api.Job{
			ID:          pointer.Of("disco"),
			Name:        pointer.Of("disco"),
			Type:        pointer.Of("service"),
			Datacenters: []string{"dc1"},
			TaskGroups: []*api.TaskGroup{{
				Name:     pointer.Of("web"),
				Count:    pointer.Of(1),
				Networks: []*api.NetworkResource{{DynamicPorts: []api.Port{{Label: "http"}}}},
				Services: []*api.Service{
					service("unset", &api.ServiceDiscovery{Prefer: "datacenter"}),
					service("explicit", &api.ServiceDiscovery{Prefer: "datacenter", OverflowFraction: pointer.Of(0.5)}),
					service("off", &api.ServiceDiscovery{Prefer: "datacenter", OverflowFraction: pointer.Of(0.0)}),
				},
				Tasks: []*api.Task{{
					Name:   "app",
					Driver: "mock_driver",
					Config: map[string]any{"run_for": "5m"},
					Resources: &api.Resources{
						CPU:      pointer.Of(100),
						MemoryMB: pointer.Of(64),
					},
				}},
			}},
		}
		want := map[string]float64{"unset": 0.35, "explicit": 0.5, "off": 0}

		// the plan of the new job already shows the resolved value
		plan, _, err := client.Jobs().Plan(job, true, nil)
		must.NoError(t, err)
		must.StrContains(t, planFieldValues(plan.Diff, "OverflowFraction"), "0.35")

		_, _, err = client.Jobs().Register(job, nil)
		must.NoError(t, err)

		stored, _, err := client.Jobs().Info("disco", nil)
		must.NoError(t, err)
		for _, svc := range stored.TaskGroups[0].Services {
			must.NotNil(t, svc.Discovery.OverflowFraction, must.Sprint(svc.Name))
			must.Eq(t, want[svc.Name], *svc.Discovery.OverflowFraction, must.Sprint(svc.Name))
		}

		// the registrations made by the client carry the resolved values
		for name, fraction := range want {
			must.Wait(t, wait.InitialSuccess(
				wait.ErrorFunc(func() error {
					regs, _, err := client.Services().Get(name, nil)
					if err != nil {
						return err
					}
					if len(regs) != 1 {
						return fmt.Errorf("%d registrations of %s", len(regs), name)
					}
					d := regs[0].Discovery
					if d == nil || d.OverflowFraction == nil || *d.OverflowFraction != fraction {
						return fmt.Errorf("registration of %s has discovery %+v", name, d)
					}
					return nil
				}),
				wait.Timeout(60*time.Second),
				wait.Gap(250*time.Millisecond),
			))
		}

		// submitting the unchanged job again does not change it
		plan, _, err = client.Jobs().Plan(job, true, nil)
		must.NoError(t, err)
		must.Eq(t, "None", plan.Diff.Type)
	})
}

// planFieldValues returns the new values of the fields with the given name in
// a job diff, joined with commas.
func planFieldValues(diff *api.JobDiff, name string) string {
	var values []string
	var walk func(objects []*api.ObjectDiff, fields []*api.FieldDiff)
	walk = func(objects []*api.ObjectDiff, fields []*api.FieldDiff) {
		for _, f := range fields {
			if f.Name == name {
				values = append(values, f.New)
			}
		}
		for _, o := range objects {
			walk(o.Objects, o.Fields)
		}
	}
	walk(diff.Objects, diff.Fields)
	for _, tg := range diff.TaskGroups {
		walk(tg.Objects, tg.Fields)
		for _, task := range tg.Tasks {
			walk(task.Objects, task.Fields)
		}
	}
	return strings.Join(values, ",")
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
