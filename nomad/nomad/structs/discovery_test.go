// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/shoenig/test/must"
)

func TestServiceDiscovery_Validate(t *testing.T) {
	ci.Parallel(t)

	var nilDiscovery *ServiceDiscovery
	must.NoError(t, nilDiscovery.Validate())

	valid := &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter, MinLocal: 2, OverflowFraction: pointer.Of(0.2), Failover: []string{"eu", "us"}}
	must.NoError(t, valid.Validate())

	err := (&ServiceDiscovery{Prefer: "rack", MinLocal: -1, OverflowFraction: pointer.Of(2.0), Failover: []string{"eu", "eu", ""}}).Validate()
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
	// the overflow fraction: unset is valid and differs from an explicit
	// value; the copy is deep
	unset := &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter}
	must.NoError(t, unset.Validate())
	must.Eq(t, DefaultDiscoveryOverflowFraction, unset.EffectiveOverflowFraction())
	explicit := &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter, OverflowFraction: pointer.Of(DefaultDiscoveryOverflowFraction)}
	must.False(t, unset.Equal(explicit))
	must.True(t, explicit.Equal(explicit.Copy()))
	cp := explicit.Copy()
	*cp.OverflowFraction = 0.7
	must.Eq(t, DefaultDiscoveryOverflowFraction, *explicit.OverflowFraction)
	must.ErrorContains(t, (&ServiceDiscovery{OverflowFraction: pointer.Of(-0.1)}).Validate(), "overflow_fraction")
	must.Eq(t, 0.0, (&ServiceDiscovery{OverflowFraction: pointer.Of(0.0)}).EffectiveOverflowFraction())

	// SetDefaultOverflowFraction fills in only an unset value
	unset.SetDefaultOverflowFraction(0.3)
	must.Eq(t, pointer.Of(0.3), unset.OverflowFraction)
	unset.SetDefaultOverflowFraction(0.4)
	must.Eq(t, pointer.Of(0.3), unset.OverflowFraction)
	off := &ServiceDiscovery{OverflowFraction: pointer.Of(0.0)}
	off.SetDefaultOverflowFraction(0.3)
	must.Eq(t, pointer.Of(0.0), off.OverflowFraction)
	var nilDisc *ServiceDiscovery
	nilDisc.SetDefaultOverflowFraction(0.3)
}

// Jobs and registrations stored while OverflowFraction was a float64 decode
// with the value they were stored with, including an explicit 0; a nil
// fraction round-trips as nil.
func TestServiceDiscovery_MsgpackCompat(t *testing.T) {
	ci.Parallel(t)

	type oldServiceDiscovery struct {
		Prefer           string
		MinLocal         int
		OverflowFraction float64
		Failover         []string
		IncludeNotReady  bool
	}
	for _, f := range []float64{0.2, 0} {
		var buf bytes.Buffer
		must.NoError(t, codec.NewEncoder(&buf, MsgpackHandle).Encode(&oldServiceDiscovery{Prefer: "datacenter", OverflowFraction: f}))
		var d ServiceDiscovery
		must.NoError(t, codec.NewDecoder(&buf, MsgpackHandle).Decode(&d))
		must.Eq(t, &ServiceDiscovery{Prefer: "datacenter", OverflowFraction: pointer.Of(f)}, &d)
	}

	for _, in := range []*ServiceDiscovery{{Prefer: "datacenter"}, {Prefer: "datacenter", OverflowFraction: pointer.Of(0.0)}} {
		var buf bytes.Buffer
		must.NoError(t, codec.NewEncoder(&buf, MsgpackHandle).Encode(in))
		var out ServiceDiscovery
		must.NoError(t, codec.NewDecoder(&buf, MsgpackHandle).Decode(&out))
		must.Eq(t, in, &out)
	}
}

func TestService_ValidateDiscovery(t *testing.T) {
	ci.Parallel(t)

	service := &Service{
		Name:      "api",
		PortLabel: "http",
		Provider:  ServiceProviderNomad,
		Discovery: &ServiceDiscovery{Prefer: DiscoveryPreferDatacenter, MinLocal: 2},
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

	added := serviceDiscoveryDiff(nil, &ServiceDiscovery{Prefer: "datacenter", Failover: []string{"eu"}}, false)
	must.Eq(t, DiffTypeAdded, added.Type)

	edited := serviceDiscoveryDiff(
		&ServiceDiscovery{Prefer: "datacenter", Failover: []string{"eu"}},
		&ServiceDiscovery{Prefer: "datacenter", Failover: []string{"eu", "us"}}, false)
	must.Eq(t, DiffTypeEdited, edited.Type)
	must.Len(t, 1, edited.Fields)
	must.Eq(t, "Failover", edited.Fields[0].Name)

	// the overflow fraction is a pointer, which primitiveObjectDiff skips
	fraction := serviceDiscoveryDiff(
		&ServiceDiscovery{Prefer: "datacenter", OverflowFraction: pointer.Of(0.2)},
		&ServiceDiscovery{Prefer: "datacenter", OverflowFraction: pointer.Of(0.35)}, false)
	must.Eq(t, DiffTypeEdited, fraction.Type)
	must.Eq(t, []*FieldDiff{{Type: DiffTypeEdited, Name: "OverflowFraction", Old: "0.2", New: "0.35"}}, fraction.Fields)

	fraction = serviceDiscoveryDiff(
		&ServiceDiscovery{Prefer: "datacenter"},
		&ServiceDiscovery{Prefer: "datacenter", OverflowFraction: pointer.Of(0.0)}, true)
	must.Eq(t, DiffTypeEdited, fraction.Type)
	var names []string
	for _, f := range fraction.Fields {
		names = append(names, f.Name)
		if f.Name == "OverflowFraction" {
			must.Eq(t, DiffTypeAdded, f.Type)
			must.Eq(t, "0", f.New)
		} else {
			must.Eq(t, DiffTypeNone, f.Type, must.Sprint(f.Name))
		}
	}
	must.Eq(t, []string{"Failover", "OverflowFraction"}, names)

	sDiff := serviceDiff(
		&Service{Name: "api", Discovery: &ServiceDiscovery{Prefer: "none"}},
		&Service{Name: "api", Discovery: &ServiceDiscovery{Prefer: "datacenter"}}, false)
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
		Checks:    []*ServiceRegistrationCheck{{Name: "c", Status: ServiceHealthPassing, Output: "ok"}},
		Meta:      map[string]string{"k": "v"},
		Discovery: &ServiceDiscovery{Prefer: "datacenter"},
	}
	b := a.Copy()
	b.Checks[0].Output = "still ok"
	must.True(t, a.HealthEqual(b))
	must.False(t, a.Equal(b))

	b.Checks[0].Status = ServiceHealthCritical
	must.False(t, a.HealthEqual(b))
	must.True(t, a.EqualExceptHealth(b))

	b.Datacenter = "dc2"
	must.False(t, a.EqualExceptHealth(b))

	// the copy is deep
	b.Meta["k"] = "w"
	b.Discovery.Prefer = "none"
	must.Eq(t, "v", a.Meta["k"])
	must.Eq(t, "datacenter", a.Discovery.Prefer)

	a.StripNorcaFields()
	must.Eq(t, "", a.Health)
	must.Nil(t, a.Checks)
	must.Nil(t, a.Discovery)
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
		{ModifyIndex: 5, Discovery: &ServiceDiscovery{Prefer: "datacenter"}},
		{ModifyIndex: 9, Discovery: &ServiceDiscovery{Prefer: "datacenter"}},
		{ModifyIndex: 12},
	}
	must.Eq(t, "datacenter", EffectiveDiscovery(regs).Prefer)
}
