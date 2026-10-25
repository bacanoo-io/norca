// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/hashicorp/nomad/controller"
)

// Kind is the custom object type maintained by this operator. Objects live
// at crd/norca-io/LoadBalancer/<name> (Variables storage), or are native
// objects of the kind norca.io/LoadBalancer (see loadbalancer.kind.hcl).
var Kind = controller.Kind{Group: "norca.io", Kind: "LoadBalancer"}

// DefaultClass is the provider used when the spec does not name one.
const DefaultClass = "nomad"

// Spec is the desired state of a load balancer.
type Spec struct {
	// Service is the name of the Nomad service (in the object's namespace)
	// whose healthy instances are the targets.
	Service string `json:"service"`

	// Class selects the provider that implements the load balancer
	// (default "nomad").
	Class string `json:"class,omitempty"`

	// Port is the frontend port of the load balancer.
	Port int `json:"port"`

	// Internal load balancers are reachable only inside the cluster
	// network.
	Internal bool `json:"internal,omitempty"`

	// Replicas is the number of load balancer instances of providers that
	// run them as Nomad jobs (default 1).
	Replicas int `json:"replicas,omitempty"`

	// Datacenters of providers that run Nomad jobs (default all).
	Datacenters []string `json:"datacenters,omitempty"`
}

// Defaults fills in the default values.
func (s *Spec) Defaults() {
	if s.Class == "" {
		s.Class = DefaultClass
	}
	if s.Replicas == 0 {
		s.Replicas = 1
	}
	if len(s.Datacenters) == 0 {
		s.Datacenters = []string{"*"}
	}
}

// Validate checks the spec.
func (s *Spec) Validate() error {
	var errs []error
	if s.Service == "" {
		errs = append(errs, errors.New("service is required"))
	}
	if s.Port < 1 || s.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d is out of range", s.Port))
	}
	if s.Replicas < 1 || s.Replicas > 16 {
		errs = append(errs, fmt.Errorf("replicas must be between 1 and 16, got %d", s.Replicas))
	}
	if slices.Contains(s.Datacenters, "") {
		errs = append(errs, errors.New("datacenters must not be empty strings"))
	}
	return errors.Join(errs...)
}

// Status is the observed state of a load balancer.
type Status struct {
	controller.CommonStatus

	// Class is the provider that implements the load balancer.
	Class string `json:"class,omitempty"`

	// Addresses are the frontend addresses ("ip:port").
	Addresses []string `json:"addresses,omitempty"`

	// Targets are the healthy instances of the service ("ip:port").
	Targets []string `json:"targets,omitempty"`

	// Resource identifies what the provider created (for the nomad class,
	// the job ID).
	Resource string `json:"resource,omitempty"`
}

// Target is a healthy instance of a service.
type Target struct {
	Address string
	Port    int
	AllocID string
}

func (t Target) String() string { return net.JoinHostPort(t.Address, strconv.Itoa(t.Port)) }
