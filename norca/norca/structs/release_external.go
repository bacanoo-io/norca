// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// ReleaseExternalService is an external service of a release: the
// declaration of "norca service apply" (the native API form) with the
// namespace it is registered in.
type ReleaseExternalService struct {
	// Namespace of the registration; empty means the namespace of the
	// release.
	Namespace string `json:",omitempty"`

	Service *ExternalServiceSpec
}

// ExternalServiceSpec is the native API form of an external service
// registration, without the settings of the node that registers it.
type ExternalServiceSpec struct {
	// ID of the registration; defaults to the name.
	ID string `json:",omitempty"`

	// Name of the service.
	Name string

	// Kind is address (default) or alias.
	Kind string `json:",omitempty"`

	Address string `json:",omitempty"`
	Target  string `json:",omitempty"`
	Port    int    `json:",omitempty"`

	Tags       []string          `json:",omitempty"`
	Meta       map[string]string `json:",omitempty"`
	Datacenter string            `json:",omitempty"`

	Checks    []*ExternalServiceCheck `json:",omitempty"`
	CheckedBy *ExternalCheckedBy      `json:",omitempty"`

	// DeregisterCriticalAfter is how long the service may stay critical
	// before it is deregistered; nil and zero keep it.
	DeregisterCriticalAfter *time.Duration `json:",omitempty"`
}

// Copy returns a deep copy of s.
func (s *ExternalServiceSpec) Copy() *ExternalServiceSpec {
	if s == nil {
		return nil
	}
	ns := *s
	ns.Tags = slices.Clone(s.Tags)
	ns.Meta = maps.Clone(s.Meta)
	ns.CheckedBy = s.CheckedBy.Copy()
	if s.Checks != nil {
		ns.Checks = make([]*ExternalServiceCheck, len(s.Checks))
		for i, c := range s.Checks {
			ns.Checks[i] = c.Copy()
		}
	}
	if s.DeregisterCriticalAfter != nil {
		d := *s.DeregisterCriticalAfter
		ns.DeregisterCriticalAfter = &d
	}
	return &ns
}

// Copy returns a deep copy of r.
func (r *ReleaseExternalService) Copy() *ReleaseExternalService {
	if r == nil {
		return nil
	}
	return &ReleaseExternalService{Namespace: r.Namespace, Service: r.Service.Copy()}
}

// Registration builds the registration of a declared external service: source
// apply, kept until deregistered unless the spec says otherwise, and its
// http and tcp checks run by a node that the servers choose. The checks are
// canonicalized and the registration validated. The namespace is the one of
// the request.
func (s *ExternalServiceSpec) Registration() (*ServiceRegistration, error) {
	kind := s.Kind
	if kind == "" {
		kind = ExternalServiceKindAddress
		if s.Target != "" && s.Address == "" {
			kind = ExternalServiceKindAlias
		}
	}
	address := s.Address
	if kind == ExternalServiceKindAlias {
		if s.Target != "" && s.Address != "" && s.Target != s.Address {
			return nil, fmt.Errorf("an alias has a target, not an address")
		}
		if s.Target != "" {
			address = s.Target
		}
	}

	id := s.ID
	if id == "" {
		id = s.Name
	}

	ext := &ServiceExternal{
		Kind:      kind,
		Source:    ExternalServiceSourceApply,
		CheckedBy: s.CheckedBy.Copy(),
	}
	if s.DeregisterCriticalAfter != nil {
		ext.DeregisterCriticalAfter = *s.DeregisterCriticalAfter
	}

	used := make(map[string]int)
	for _, c := range s.Checks {
		if c == nil {
			continue
		}
		check := c.Copy()
		check.Type = strings.ToLower(check.Type)
		if check.ID == "" {
			used[check.Type]++
			check.ID = check.Type
			if n := used[check.Type]; n > 1 {
				check.ID = fmt.Sprintf("%s:%d", check.Type, n)
			}
		}
		if check.Name == "" {
			check.Name = fmt.Sprintf("%s %s check", s.Name, check.Type)
		}
		if check.InitialStatus == "" {
			check.InitialStatus = ServiceHealthPassing
		}
		check.Canonicalize()
		ext.Checks = append(ext.Checks, check)
	}
	if ext.HasNodeChecks() && ext.CheckedBy == nil {
		return nil, fmt.Errorf("the http and tcp checks of a declared service need checked_by")
	}

	reg := &ServiceRegistration{
		ID:          id,
		ServiceName: s.Name,
		Datacenter:  s.Datacenter,
		Tags:        slices.Clone(s.Tags),
		Address:     address,
		Port:        s.Port,
		Meta:        maps.Clone(s.Meta),
		External:    ext,
	}
	if err := reg.ValidateExternal(); err != nil {
		return nil, err
	}
	return reg, nil
}

// ExternalServiceSpecFromRegistration is the inverse of Registration: the
// declaration of a registration.
func ExternalServiceSpecFromRegistration(reg *ServiceRegistration) *ExternalServiceSpec {
	spec := &ExternalServiceSpec{
		ID:         reg.ID,
		Name:       reg.ServiceName,
		Kind:       reg.External.Kind,
		Port:       reg.Port,
		Datacenter: reg.Datacenter,
		CheckedBy:  reg.External.CheckedBy.Copy(),
	}
	if len(reg.Tags) > 0 {
		spec.Tags = slices.Clone(reg.Tags)
	}
	if len(reg.Meta) > 0 {
		spec.Meta = maps.Clone(reg.Meta)
	}
	if reg.External.Kind == ExternalServiceKindAlias {
		spec.Target = reg.Address
	} else {
		spec.Address = reg.Address
	}
	for _, c := range reg.External.Checks {
		spec.Checks = append(spec.Checks, c.Copy())
	}
	d := reg.External.DeregisterCriticalAfter
	spec.DeregisterCriticalAfter = &d
	return spec
}

// Normalized returns the declaration with the defaults applied, as it reads
// back from the registration it makes.
func (s *ExternalServiceSpec) Normalized() (*ExternalServiceSpec, error) {
	reg, err := s.Registration()
	if err != nil {
		return nil, err
	}
	return ExternalServiceSpecFromRegistration(reg), nil
}
