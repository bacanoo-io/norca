// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"maps"
	"slices"
)

// The health states of a service registration and of its checks. They follow
// the states of Consul, so the Consul-compatible layer can expose them as they
// are.
const (
	ServiceHealthPassing  = "passing"
	ServiceHealthWarning  = "warning"
	ServiceHealthCritical = "critical"

	// ServiceCheckOutputLimit is the number of bytes of check output kept in
	// a registration.
	ServiceCheckOutputLimit = 256
)

// ServiceRegistrationCheck is the state of one check of a service instance, as
// last reported by the client running it.
type ServiceRegistrationCheck struct {
	// ID of the check; set for the checks of external services only.
	ID string

	// Name of the check.
	Name string

	// Role of the check (startup, readiness, liveness) or empty.
	Role string

	// Status is passing, warning or critical. A check that has not run yet
	// is critical, as in Consul.
	Status string

	// Output is the beginning of the output of the check at the time its
	// status last changed.
	Output string
}

// Copy returns a copy of c.
func (c *ServiceRegistrationCheck) Copy() *ServiceRegistrationCheck {
	if c == nil {
		return nil
	}
	nc := *c
	return &nc
}

// Equal returns whether c and o are equal, including the output.
func (c *ServiceRegistrationCheck) Equal(o *ServiceRegistrationCheck) bool {
	if c == nil || o == nil {
		return c == o
	}
	return *c == *o
}

// Healthy returns whether the instance is healthy: it is ready and none of
// its checks that count toward health is critical. Registrations written by
// clients or servers that predate the health state have no Health, in which
// case Ready alone decides.
func (s *ServiceRegistration) Healthy() bool {
	if s == nil || !s.Ready {
		return false
	}
	return s.Health != ServiceHealthCritical
}

// Discoverable returns whether discovery that filters for healthy instances
// returns the instance: it is healthy, or its service publishes instances that
// are not ready.
func (s *ServiceRegistration) Discoverable() bool {
	if s.Healthy() {
		return true
	}
	return s != nil && s.Discovery != nil && s.Discovery.IncludeNotReady
}

// HealthEqual returns whether s and o report the same health: the same Ready
// flag, aggregate state and check states. The output of the checks is not
// compared, so a check whose output changes on every run does not cause
// writes.
func (s *ServiceRegistration) HealthEqual(o *ServiceRegistration) bool {
	if s == nil || o == nil {
		return s == o
	}
	if s.Ready != o.Ready || s.Health != o.Health || len(s.Checks) != len(o.Checks) {
		return false
	}
	for i := range s.Checks {
		a, b := s.Checks[i], o.Checks[i]
		if a == nil || b == nil {
			if a != b {
				return false
			}
			continue
		}
		if a.ID != b.ID || a.Name != b.Name || a.Role != b.Role || a.Status != b.Status {
			return false
		}
	}
	return true
}

// EqualExceptHealth returns whether s and o are equal apart from their health
// fields (Ready, Health, Checks and HealthUpdatedAt).
func (s *ServiceRegistration) EqualExceptHealth(o *ServiceRegistration) bool {
	if s == nil || o == nil {
		return s == o
	}
	c := o.Copy()
	c.Ready = s.Ready
	c.Health = s.Health
	c.Checks = s.Checks
	c.HealthUpdatedAt = s.HealthUpdatedAt
	return s.Equal(c)
}

// CopyHealthFrom sets the health fields of s from o.
func (s *ServiceRegistration) CopyHealthFrom(o *ServiceRegistration) {
	s.Ready = o.Ready
	s.Health = o.Health
	s.Checks = copyRegistrationChecks(o.Checks)
	s.HealthUpdatedAt = o.HealthUpdatedAt
}

// StripNorcaFields removes the fields that servers predating the health-based
// catalog do not know, so registrations stay identical on every server of a
// region during an upgrade.
func (s *ServiceRegistration) StripNorcaFields() {
	s.Health = ""
	s.Checks = nil
	s.HealthUpdatedAt = 0
	s.Meta = nil
	s.Discovery = nil
	s.External = nil
}

func copyRegistrationChecks(checks []*ServiceRegistrationCheck) []*ServiceRegistrationCheck {
	if checks == nil {
		return nil
	}
	out := make([]*ServiceRegistrationCheck, len(checks))
	for i, c := range checks {
		out[i] = c.Copy()
	}
	return out
}

func registrationChecksEqual(a, b []*ServiceRegistrationCheck) bool {
	return slices.EqualFunc(a, b, func(x, y *ServiceRegistrationCheck) bool { return x.Equal(y) })
}

func registrationMetaEqual(a, b map[string]string) bool {
	return maps.Equal(a, b)
}

// TruncateCheckOutput returns the beginning of the output of a check, as kept
// in a registration.
func TruncateCheckOutput(output string) string {
	if len(output) <= ServiceCheckOutputLimit {
		return output
	}
	// do not cut a multi-byte character in two
	cut := ServiceCheckOutputLimit
	for cut > 0 && output[cut]&0xC0 == 0x80 {
		cut--
	}
	return output[:cut]
}
