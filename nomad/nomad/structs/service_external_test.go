// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func externalReg() *ServiceRegistration {
	return &ServiceRegistration{
		ID: "db", ServiceName: "db", Namespace: "default", Address: "10.0.0.5", Port: 5432,
		External: &ServiceExternal{
			Kind: ExternalServiceKindAddress,
			Checks: []*ExternalServiceCheck{
				{ID: "ttl", Name: "ttl", Type: ExternalCheckTypeTTL, TTL: 10 * time.Second},
			},
		},
	}
}

func TestServiceRegistration_ValidateExternal(t *testing.T) {
	ci.Parallel(t)

	must.NoError(t, externalReg().ValidateExternal())

	cases := map[string]func(*ServiceRegistration){
		"no id":           func(r *ServiceRegistration) { r.ID = "" },
		"bad name":        func(r *ServiceRegistration) { r.ServiceName = "a_b" },
		"no address":      func(r *ServiceRegistration) { r.Address = "" },
		"bad address":     func(r *ServiceRegistration) { r.Address = "not a host" },
		"bad port":        func(r *ServiceRegistration) { r.Port = 70000 },
		"alloc":           func(r *ServiceRegistration) { r.AllocID = "x" },
		"bad kind":        func(r *ServiceRegistration) { r.External.Kind = "cname" },
		"alias to ip":     func(r *ServiceRegistration) { r.External.Kind = ExternalServiceKindAlias },
		"short ttl":       func(r *ServiceRegistration) { r.External.Checks[0].TTL = time.Millisecond },
		"bad check type":  func(r *ServiceRegistration) { r.External.Checks[0].Type = "script" },
		"no check id":     func(r *ServiceRegistration) { r.External.Checks[0].ID = "" },
		"bad status":      func(r *ServiceRegistration) { r.External.Checks[0].InitialStatus = "ok" },
		"negative dca":    func(r *ServiceRegistration) { r.External.DeregisterCriticalAfter = -time.Second },
		"duplicate check": func(r *ServiceRegistration) { r.External.Checks = append(r.External.Checks, r.External.Checks[0]) },
		"bad http": func(r *ServiceRegistration) {
			r.External.Checks[0] = &ExternalServiceCheck{ID: "h", Type: ExternalCheckTypeHTTP, HTTP: "ftp://x", Interval: time.Second}
		},
		"bad tcp": func(r *ServiceRegistration) {
			r.External.Checks[0] = &ExternalServiceCheck{ID: "t", Type: ExternalCheckTypeTCP, TCP: "nohost", Interval: time.Second}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			reg := externalReg()
			mutate(reg)
			must.Error(t, reg.ValidateExternal())
		})
	}

	// an alias points to a DNS name
	alias := externalReg()
	alias.External.Kind = ExternalServiceKindAlias
	alias.Address = "szamlazas.c9xk2.eu-central-1.rds.amazonaws.com"
	must.NoError(t, alias.ValidateExternal())

	// http and tcp checks get their defaults
	check := &ExternalServiceCheck{ID: "h", Type: "HTTP", HTTP: "http://10.0.0.5/health", Timeout: time.Minute}
	check.Canonicalize()
	must.Eq(t, ExternalCheckTypeHTTP, check.Type)
	must.Eq(t, "GET", check.Method)
	must.Eq(t, DefaultExternalCheckInterval, check.Interval)
	must.Eq(t, DefaultExternalCheckInterval, check.Timeout)
}

func TestServiceRegistration_ExternalCopyEqualHealth(t *testing.T) {
	ci.Parallel(t)

	reg := externalReg()
	reg.External.Checks = append(reg.External.Checks, &ExternalServiceCheck{
		ID: "h", Type: ExternalCheckTypeHTTP, HTTP: "http://x/", Header: map[string][]string{"A": {"1"}}})
	cp := reg.Copy()
	must.True(t, reg.Equal(cp))
	cp.External.Checks[1].Header["A"][0] = "2"
	must.False(t, reg.Equal(cp))
	must.Eq(t, "1", reg.External.Checks[1].Header["A"][0])
	must.True(t, reg.External.HasTTLChecks())
	must.True(t, reg.External.HasNodeChecks())

	now := time.Unix(100, 0)
	reg.Checks = []*ServiceRegistrationCheck{
		{ID: "ttl", Status: ServiceHealthPassing},
		{ID: "h", Status: ServiceHealthWarning},
	}
	reg.SetExternalHealth(nil, now)
	must.True(t, reg.Ready)
	must.Eq(t, ServiceHealthWarning, reg.Health)
	must.Eq(t, now.UnixNano(), reg.HealthUpdatedAt)
	must.True(t, reg.Discoverable())

	next := reg.Copy()
	next.Checks[0].Status = ServiceHealthCritical
	next.SetExternalHealth(reg, now.Add(time.Minute))
	must.Eq(t, ServiceHealthCritical, next.Health)
	must.Eq(t, now.Add(time.Minute).UnixNano(), next.HealthUpdatedAt)
	must.False(t, next.Discoverable())

	// the same aggregate keeps the time of the last change
	same := next.Copy()
	same.Checks[1].Status = ServiceHealthPassing
	same.SetExternalHealth(next, now.Add(2*time.Minute))
	must.Eq(t, next.HealthUpdatedAt, same.HealthUpdatedAt)

	// stripping for old servers removes the external settings
	next.StripNorcaFields()
	must.Nil(t, next.External)
}
