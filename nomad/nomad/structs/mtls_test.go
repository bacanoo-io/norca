// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"fmt"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestServiceMTLS_Validate(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name string
		mtls *ServiceMTLS
		err  string
	}{
		{name: "nil"},
		{name: "app", mtls: &ServiceMTLS{Mode: MTLSModeApp}},
		{name: "default mode", mtls: &ServiceMTLS{}},
		{
			name: "sidecar",
			mtls: &ServiceMTLS{Mode: MTLSModeSidecar, Upstreams: []*ServiceMTLSUpstream{
				{Name: "db", Service: "postgres", LocalPort: 5432},
				{Name: "remote", Service: "redis", Namespace: "cache", Region: "eu", Gateway: "gw", LocalPort: 6379},
			}},
		},
		{name: "sidecar without upstreams", mtls: &ServiceMTLS{Mode: MTLSModeSidecar}},
		{name: "gateway", mtls: &ServiceMTLS{Mode: MTLSModeGateway}},
		{
			name: "upstream in gateway mode",
			mtls: &ServiceMTLS{Mode: MTLSModeGateway, Upstreams: []*ServiceMTLSUpstream{{Name: "db", Service: "postgres", LocalPort: 5432}}},
			err:  "only used in mode",
		},
		{
			name: "upstream without local port",
			mtls: &ServiceMTLS{Mode: MTLSModeSidecar, Upstreams: []*ServiceMTLSUpstream{{Name: "db", Service: "postgres"}}},
			err:  "local_port must be between 1 and 65535",
		},
		{
			name: "duplicate local port",
			mtls: &ServiceMTLS{Mode: MTLSModeSidecar, Upstreams: []*ServiceMTLSUpstream{
				{Name: "a", Service: "postgres", LocalPort: 5432}, {Name: "b", Service: "redis", LocalPort: 5432}}},
			err: "use the same local_port",
		},
		{
			name: "gateway without region",
			mtls: &ServiceMTLS{Mode: MTLSModeSidecar, Upstreams: []*ServiceMTLSUpstream{{Name: "a", Service: "postgres", Gateway: "gw", LocalPort: 5432}}},
			err:  "gateway is only used with region",
		},
		{
			name: "invalid upstream fields",
			mtls: &ServiceMTLS{Mode: MTLSModeSidecar, Upstreams: []*ServiceMTLSUpstream{{Name: "a/b", Service: "${x}", Namespace: "n s", Region: "e.u", LocalPort: 5432}}},
			err:  "may only contain",
		},
		{
			name: "upstream in app mode",
			mtls: &ServiceMTLS{Mode: MTLSModeApp, Upstreams: []*ServiceMTLSUpstream{{Name: "db", Service: "postgres", LocalPort: 5432}}},
			err:  "only used in mode",
		},
		{
			name: "invalid upstream",
			mtls: &ServiceMTLS{Mode: MTLSModeSidecar, Upstreams: []*ServiceMTLSUpstream{{Name: "db", LocalPort: 70000}}},
			err:  "requires a service",
		},
		{name: "bad mode", mtls: &ServiceMTLS{Mode: "magic"}, err: "mtls mode must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.mtls.Canonicalize()
			err := tc.mtls.Validate()
			if tc.err == "" {
				must.NoError(t, err)
			} else {
				must.ErrorContains(t, err, tc.err)
			}
		})
	}
}

func TestService_MTLSProvider(t *testing.T) {
	ci.Parallel(t)

	s := &Service{Name: "api", Provider: ServiceProviderConsul, MTLS: &ServiceMTLS{}}
	s.Canonicalize("job", "group", "task", "default")
	must.ErrorContains(t, s.Validate(), "mtls may only be set on services with provider nomad")

	s = &Service{Name: "api", Provider: ServiceProviderNomad, MTLS: &ServiceMTLS{}}
	s.Canonicalize("job", "group", "task", "default")
	must.NoError(t, s.Validate())
	must.Eq(t, MTLSModeApp, s.MTLS.Mode)

	c := s.Copy()
	must.True(t, c.Equal(s))
	c.MTLS.Mode = MTLSModeSidecar
	must.False(t, c.Equal(s))
	must.Eq(t, MTLSModeApp, s.MTLS.Mode)
}

func TestTaskGroup_MTLSServices(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{
		Services: []*Service{{Name: "plain"}, {Name: "api", MTLS: &ServiceMTLS{}}},
		Tasks:    []*Task{{Name: "t", Services: []*Service{{Name: "admin", MTLS: &ServiceMTLS{}}}}},
	}
	services := tg.MTLSServices()
	must.Len(t, 2, services)
	must.Eq(t, "api", services[0].Name)
	must.Eq(t, "admin", services[1].Name)
	must.True(t, tg.UsesMTLS())
	must.True(t, (&Job{TaskGroups: []*TaskGroup{tg}}).UsesMTLS())
	must.False(t, (&TaskGroup{}).UsesMTLS())
	must.False(t, (*TaskGroup)(nil).UsesMTLS())
}

func TestMTLS_SPIFFEAndDNSNames(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "spiffe://global.nomad/ns/prod/job/web/group/frontend",
		MTLSWorkloadSPIFFEID("global.nomad", "prod", "web", "frontend"))

	must.NoError(t, ValidateSPIFFESegment("job", "web-1.v2_x"))
	must.Error(t, ValidateSPIFFESegment("job", "web/periodic-123"))
	must.Error(t, ValidateSPIFFESegment("job", ".."))
	must.Error(t, ValidateSPIFFESegment("group", "with space"))
	must.NoError(t, ValidateSPIFFETrustDomain("eu-west.nomad"))
	must.Error(t, ValidateSPIFFETrustDomain("EU.nomad"))
	must.Error(t, ValidateSPIFFETrustDomain(""))

	must.Eq(t, []string{"api.service.prod.nomad", "api.service.prod.eu.nomad"},
		MTLSServiceDNSNames("API", "prod", "eu"))
	must.Eq(t, []string{"api.service.default.nomad", "api.service.default.eu.nomad", "api.service.nomad"},
		MTLSServiceDNSNames("api", "default", "eu"))

	must.True(t, IsValidMTLSServiceName("api-v2"))
	must.False(t, IsValidMTLSServiceName("${NOMAD_JOB_NAME}-api"))
}

func TestServiceIntention_Validate(t *testing.T) {
	ci.Parallel(t)

	i := &ServiceIntention{Source: "web", Destination: "api", Action: "ALLOW"}
	i.Canonicalize()
	must.Eq(t, DefaultNamespace, i.Namespace)
	must.Eq(t, DefaultNamespace, i.SourceNamespace)
	must.Eq(t, ServiceIntentionActionAllow, i.Action)
	must.NoError(t, i.Validate())
	must.Eq(t, "default/web -> default/api", i.Key())

	wild := &ServiceIntention{Namespace: "prod", SourceNamespace: "*", Source: "*", Destination: "*", Action: "deny"}
	wild.Canonicalize()
	must.NoError(t, wild.Validate())
	must.False(t, wild.SameKey(i))

	for _, bad := range []*ServiceIntention{
		{Source: "web", Destination: "api", Action: "maybe"},
		{Source: "", Destination: "api", Action: "allow"},
		{Source: "web", Destination: "a.b", Action: "allow"},
		{Namespace: "*", Source: "web", Destination: "api", Action: "allow"},
		{SourceNamespace: "bad ns", Source: "web", Destination: "api", Action: "allow"},
	} {
		bad.Canonicalize()
		must.Error(t, bad.Validate(), must.Sprintf("%#v", bad))
	}
}

func TestServiceDiff_MTLS(t *testing.T) {
	ci.Parallel(t)

	old := &Service{Name: "api", Provider: ServiceProviderNomad}
	new := old.Copy()
	new.MTLS = &ServiceMTLS{Mode: MTLSModeApp}
	diff := serviceDiff(old, new, false)
	must.NotNil(t, diff)
	var found *ObjectDiff
	for _, o := range diff.Objects {
		if o.Name == "MTLS" {
			found = o
		}
	}
	must.NotNil(t, found)
	must.Eq(t, DiffTypeAdded, found.Type)

	edited := new.Copy()
	edited.MTLS.Upstreams = []*ServiceMTLSUpstream{{Name: "db", Service: "postgres", LocalPort: 5432}}
	d := serviceMTLSDiff(new.MTLS, edited.MTLS, false)
	must.NotNil(t, d)
	must.Eq(t, DiffTypeEdited, d.Type)
	must.Len(t, 1, d.Objects)
	must.Nil(t, serviceMTLSDiff(new.MTLS, new.MTLS.Copy(), false))

	// the fields of upstreams are diffed
	region := edited.Copy()
	region.MTLS.Upstreams[0].Region = "eu"
	d = serviceMTLSDiff(edited.MTLS, region.MTLS, false)
	must.NotNil(t, d)
	must.Len(t, 1, d.Objects)
	must.Len(t, 1, d.Objects[0].Fields)
	must.Eq(t, "Region", d.Objects[0].Fields[0].Name)
	must.Eq(t, "eu", d.Objects[0].Fields[0].New)
}

func TestTaskGroup_ValidateMTLSProxies(t *testing.T) {
	ci.Parallel(t)

	group := func(mode string, services ...*Service) *TaskGroup {
		return &TaskGroup{
			Name:     "g",
			Networks: Networks{{Mode: mode, ReservedPorts: []Port{{Label: "gw", Value: 8443}}}},
			Services: services,
			Tasks:    []*Task{{Name: "app"}},
		}
	}
	sidecar := func(name, port string, localPorts ...int) *Service {
		m := &ServiceMTLS{Mode: MTLSModeSidecar}
		for i, p := range localPorts {
			m.Upstreams = append(m.Upstreams, &ServiceMTLSUpstream{Name: fmt.Sprint("u", i), Service: "db", LocalPort: p})
		}
		return &Service{Name: name, PortLabel: port, Provider: ServiceProviderNomad, MTLS: m}
	}
	gateway := &Service{Name: "mtls-gateway", PortLabel: "gw", Provider: ServiceProviderNomad, MTLS: &ServiceMTLS{Mode: MTLSModeGateway}}

	must.NoError(t, group("bridge", sidecar("api", "8080", 5432), sidecar("admin", "9090", 6379)).ValidateMTLSProxies())
	must.NoError(t, group("bridge", gateway).ValidateMTLSProxies())
	must.NoError(t, group("host", gateway).ValidateMTLSProxies())
	must.NoError(t, group("host", &Service{Name: "api", MTLS: &ServiceMTLS{Mode: MTLSModeApp}}).ValidateMTLSProxies())

	must.ErrorContains(t, group("host", sidecar("api", "8080")).ValidateMTLSProxies(), "requires bridge networking")
	must.ErrorContains(t, group("bridge", sidecar("api", "")).ValidateMTLSProxies(), "requires the port of the application")
	must.ErrorContains(t, group("bridge", sidecar("api", "8080", 5432), sidecar("admin", "9090", 5432)).ValidateMTLSProxies(),
		"use the same upstream local_port")
	must.ErrorContains(t, group("bridge", sidecar("${NOMAD_JOB_NAME}", "8080")).ValidateMTLSProxies(), "not interpolated")
	badGateway := gateway.Copy()
	badGateway.PortLabel = "nope"
	must.ErrorContains(t, group("bridge", badGateway).ValidateMTLSProxies(), "label of a port")
	must.ErrorContains(t, group("cni/x", gateway).ValidateMTLSProxies(), "bridge or host")

	noNetwork := group("bridge", sidecar("api", "8080"))
	noNetwork.Networks = nil
	must.ErrorContains(t, noNetwork.ValidateMTLSProxies(), "exactly 1 network")

	taskLevel := group("bridge")
	taskLevel.Tasks[0].Services = []*Service{sidecar("api", "8080")}
	must.ErrorContains(t, taskLevel.ValidateMTLSProxies(), "only supported on group services")
}

func TestMTLSProxiesChanged(t *testing.T) {
	ci.Parallel(t)

	a := &TaskGroup{Services: []*Service{
		{Name: "api", PortLabel: "8080", MTLS: &ServiceMTLS{Mode: MTLSModeSidecar,
			Upstreams: []*ServiceMTLSUpstream{{Name: "db", Service: "postgres", LocalPort: 5432}}}},
		{Name: "plain", PortLabel: "http"},
	}}
	b := a.Copy()
	must.False(t, MTLSProxiesChanged(a, b))

	b.Services[1].Tags = []string{"x"}
	must.False(t, MTLSProxiesChanged(a, b))

	b.Services[0].MTLS.Upstreams[0].LocalPort = 5433
	must.True(t, MTLSProxiesChanged(a, b))

	c := a.Copy()
	c.Services[0].PortLabel = "9090"
	must.True(t, MTLSProxiesChanged(a, c))

	d := a.Copy()
	d.Services[0].MTLS.Mode = MTLSModeApp
	must.True(t, MTLSProxiesChanged(a, d))
}

func TestTaskKind_MTLS(t *testing.T) {
	ci.Parallel(t)

	must.True(t, NewTaskKind(MTLSProxyPrefix, "api").IsMTLSProxy())
	must.False(t, NewTaskKind(MTLSProxyPrefix, "api").IsMTLSGateway())
	must.True(t, NewTaskKind(MTLSGatewayPrefix, "gw").IsMTLSGateway())
	must.False(t, TaskKind(MTLSProxyPrefix+":").IsMTLSProxy())
	must.False(t, NewTaskKind(ConnectProxyPrefix, "api").IsMTLSProxy())
	must.Eq(t, "mtls-proxy-api", MTLSProxyPortLabel("api"))
}
