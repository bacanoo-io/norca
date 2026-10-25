// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestServiceDiscovery_Validate(t *testing.T) {
	ci.Parallel(t)

	var nilDiscovery *ServiceDiscovery
	must.NoError(t, nilDiscovery.Validate())

	valid := &ServiceDiscovery{Prefer: DiscoveryPreferZone, MinLocal: 2, OverflowFraction: 0.2, Failover: []string{"eu", "us"}}
	must.NoError(t, valid.Validate())

	err := (&ServiceDiscovery{Prefer: "rack", MinLocal: -1, OverflowFraction: 2, Failover: []string{"eu", "eu", ""}}).Validate()
	must.ErrorContains(t, err, "prefer must be")
	must.ErrorContains(t, err, "min_local must be non-negative")
	must.ErrorContains(t, err, "overflow_fraction must be between 0 and 1")
	must.ErrorContains(t, err, `"eu" listed more than once`)
	must.ErrorContains(t, err, "must not be empty")

	d := &ServiceDiscovery{}
	d.Canonicalize()
	must.Eq(t, DiscoveryPreferNone, d.Prefer)

	c := valid.Copy()
	must.True(t, c.Equal(valid))
	c.Failover[0] = "ap"
	must.False(t, c.Equal(valid))
	must.Eq(t, "eu", valid.Failover[0])
}

func TestService_ValidateDiscovery(t *testing.T) {
	ci.Parallel(t)

	service := &Service{
		Name:      "api",
		PortLabel: "http",
		Provider:  ServiceProviderNomad,
		Discovery: &ServiceDiscovery{Prefer: DiscoveryPreferZone, MinLocal: 2},
		Checks: []*ServiceCheck{{
			Name:                   "alive",
			Type:                   ServiceCheckHTTP,
			Path:                   "/",
			Interval:               5 * time.Second,
			Timeout:                time.Second,
			SuccessBeforePassing:   2,
			FailuresBeforeCritical: 3,
		}},
	}
	service.Canonicalize("job", "group", "task", "default")
	must.NoError(t, service.Validate())

	service.Discovery.Prefer = "rack"
	must.ErrorContains(t, service.Validate(), "prefer must be")

	service.Discovery.Prefer = DiscoveryPreferNone
	service.Checks[0].FailuresBeforeCritical = -1
	must.ErrorContains(t, service.Validate(), "failures_before_critical must be non-negative")
	service.Checks[0].FailuresBeforeCritical = 0

	consul := service.Copy()
	consul.Provider = ServiceProviderConsul
	must.ErrorContains(t, consul.Validate(), "discovery may only be set on services with provider nomad")

	// Copy and Equal include the discovery block
	other := service.Copy()
	must.True(t, other.Equal(service))
	other.Discovery.MinLocal = 5
	must.False(t, other.Equal(service))
	must.Eq(t, 2, service.Discovery.MinLocal)
}

func TestServiceDiscoveryDiff(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, serviceDiscoveryDiff(nil, nil, false))

	added := serviceDiscoveryDiff(nil, &ServiceDiscovery{Prefer: "zone", Failover: []string{"eu"}}, false)
	must.Eq(t, DiffTypeAdded, added.Type)

	edited := serviceDiscoveryDiff(
		&ServiceDiscovery{Prefer: "zone", Failover: []string{"eu"}},
		&ServiceDiscovery{Prefer: "zone", Failover: []string{"eu", "us"}}, false)
	must.Eq(t, DiffTypeEdited, edited.Type)
	must.Len(t, 1, edited.Fields)
	must.Eq(t, "Failover", edited.Fields[0].Name)

	sDiff := serviceDiff(
		&Service{Name: "api", Discovery: &ServiceDiscovery{Prefer: "none"}},
		&Service{Name: "api", Discovery: &ServiceDiscovery{Prefer: "zone"}}, false)
	must.NotNil(t, sDiff)
	must.Eq(t, "Discovery", sDiff.Objects[len(sDiff.Objects)-1].Name)
}

func TestServiceRegistration_Health(t *testing.T) {
	ci.Parallel(t)

	reg := &ServiceRegistration{ID: "a", Ready: true}
	must.True(t, reg.Healthy(), must.Sprint("registrations without health are healthy when ready"))

	reg.Health = ServiceHealthWarning
	must.True(t, reg.Healthy())

	reg.Health = ServiceHealthCritical
	must.False(t, reg.Healthy())
	must.False(t, reg.Discoverable())

	reg.Discovery = &ServiceDiscovery{IncludeNotReady: true}
	must.True(t, reg.Discoverable())

	reg.Health = ServiceHealthPassing
	reg.Ready = false
	reg.Discovery = nil
	must.False(t, reg.Healthy())

	var nilReg *ServiceRegistration
	must.False(t, nilReg.Healthy())
	must.False(t, nilReg.Discoverable())
}

func TestServiceRegistration_HealthEqual(t *testing.T) {
	ci.Parallel(t)

	a := &ServiceRegistration{
		ID: "a", ServiceName: "api", Ready: true, Health: ServiceHealthPassing,
		Checks: []*ServiceRegistrationCheck{{Name: "c", Status: ServiceHealthPassing, Output: "ok"}},
		Zone:   "z1", Meta: map[string]string{"k": "v"},
		Discovery: &ServiceDiscovery{Prefer: "zone"},
	}
	b := a.Copy()
	b.Checks[0].Output = "still ok"
	must.True(t, a.HealthEqual(b))
	must.False(t, a.Equal(b))

	b.Checks[0].Status = ServiceHealthCritical
	must.False(t, a.HealthEqual(b))
	must.True(t, a.EqualExceptHealth(b))

	b.Zone = "z2"
	must.False(t, a.EqualExceptHealth(b))

	// the copy is deep
	b.Meta["k"] = "w"
	b.Discovery.Prefer = "none"
	must.Eq(t, "v", a.Meta["k"])
	must.Eq(t, "zone", a.Discovery.Prefer)

	a.StripNorcaFields()
	must.Eq(t, "", a.Health)
	must.Nil(t, a.Checks)
	must.Nil(t, a.Discovery)
	must.Eq(t, "", a.Zone)
}

func TestTruncateCheckOutput(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "short", TruncateCheckOutput("short"))
	long := strings.Repeat("a", ServiceCheckOutputLimit-1) + "é" + "tail"
	out := TruncateCheckOutput(long)
	must.Eq(t, ServiceCheckOutputLimit-1, len(out))
}

func TestEffectiveDiscovery(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, EffectiveDiscovery(nil))
	regs := []*ServiceRegistration{
		{ModifyIndex: 5, Discovery: &ServiceDiscovery{Prefer: "zone"}},
		{ModifyIndex: 9, Discovery: &ServiceDiscovery{Prefer: "datacenter"}},
		{ModifyIndex: 12},
	}
	must.Eq(t, "datacenter", EffectiveDiscovery(regs).Prefer)
}
