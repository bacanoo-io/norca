// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
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
			name: "sidecar not implemented",
			mtls: &ServiceMTLS{Mode: MTLSModeSidecar, Upstreams: []*ServiceMTLSUpstream{{Name: "db", Service: "postgres", LocalPort: 5432}}},
			err:  `mtls mode "sidecar" is not implemented yet`,
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
}
