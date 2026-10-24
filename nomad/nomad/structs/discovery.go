// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/go-multierror"
)

// Topology preferences of the discovery block of a service. They select which
// instances discovery (DNS, templates, the API with near=self and the
// Consul-compatible layer) returns first, relative to the consumer.
const (
	// DiscoveryPreferNone applies no topology preference: every eligible
	// instance of the region is returned. It is the default.
	DiscoveryPreferNone = "none"

	// DiscoveryPreferZone prefers the instances in the zone of the consumer,
	// then those in its datacenter, then the rest of the region.
	DiscoveryPreferZone = "zone"

	// DiscoveryPreferDatacenter prefers the instances in the datacenter of the
	// consumer, then the rest of the region.
	DiscoveryPreferDatacenter = "datacenter"

	// DiscoveryPreferRegion returns every instance of the region, as "none"
	// does; it exists so the failover list can be used on its own explicitly.
	DiscoveryPreferRegion = "region"

	// DefaultDiscoveryOverflowFraction is the default fraction of all eligible
	// instances a topology tier must hold before the next tier is left out of
	// the response, so a small zone does not receive the entire traffic.
	DefaultDiscoveryOverflowFraction = 0.2
)

// Topology is the location of a consumer or instance of a service.
type Topology struct {
	Region     string
	Datacenter string
	Zone       string
}

// ServiceDiscovery is the discovery block of a service using the Nomad
// provider. It tells consumers of the native catalog how to choose among the
// instances of the service. The settings are copied into every registration of
// the service.
type ServiceDiscovery struct {
	// Prefer is the topology preference: "none" (default), "zone",
	// "datacenter" or "region".
	Prefer string

	// MinLocal is the number of eligible instances a topology tier must hold
	// before the next tier is left out.
	MinLocal int

	// OverflowFraction is the fraction of all eligible instances a topology
	// tier must hold before the next tier is left out. Zero disables the rule.
	OverflowFraction float64

	// Failover is the ordered list of regions queried when the local region
	// has no eligible instance.
	Failover []string

	// IncludeNotReady makes instances that are not healthy (or not yet ready)
	// eligible for discovery as well, for stateful clusters whose members must
	// find each other before they are ready.
	IncludeNotReady bool
}

// Copy returns a deep copy of d.
func (d *ServiceDiscovery) Copy() *ServiceDiscovery {
	if d == nil {
		return nil
	}
	nd := *d
	nd.Failover = slices.Clone(d.Failover)
	return &nd
}

// Equal returns whether d and o are equal.
func (d *ServiceDiscovery) Equal(o *ServiceDiscovery) bool {
	if d == nil || o == nil {
		return d == o
	}
	return d.Prefer == o.Prefer &&
		d.MinLocal == o.MinLocal &&
		d.OverflowFraction == o.OverflowFraction &&
		d.IncludeNotReady == o.IncludeNotReady &&
		slices.Equal(d.Failover, o.Failover)
}

// Canonicalize sets the defaults of the discovery block.
func (d *ServiceDiscovery) Canonicalize() {
	if d == nil {
		return
	}
	if d.Prefer == "" {
		d.Prefer = DiscoveryPreferNone
	}
	if len(d.Failover) == 0 {
		d.Failover = nil
	}
}

// Validate the discovery block.
func (d *ServiceDiscovery) Validate() error {
	if d == nil {
		return nil
	}
	var mErr multierror.Error

	switch d.Prefer {
	case "", DiscoveryPreferNone, DiscoveryPreferZone, DiscoveryPreferDatacenter, DiscoveryPreferRegion:
	default:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("discovery prefer must be %q, %q, %q or %q; got %q",
			DiscoveryPreferNone, DiscoveryPreferZone, DiscoveryPreferDatacenter, DiscoveryPreferRegion, d.Prefer))
	}
	if d.MinLocal < 0 {
		mErr.Errors = append(mErr.Errors, errors.New("discovery min_local must be non-negative"))
	}
	if d.OverflowFraction < 0 || d.OverflowFraction > 1 {
		mErr.Errors = append(mErr.Errors, errors.New("discovery overflow_fraction must be between 0 and 1"))
	}
	seen := make(map[string]struct{}, len(d.Failover))
	for _, region := range d.Failover {
		if region == "" {
			mErr.Errors = append(mErr.Errors, errors.New("discovery failover regions must not be empty"))
			continue
		}
		if _, ok := seen[region]; ok {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("discovery failover region %q listed more than once", region))
		}
		seen[region] = struct{}{}
	}
	return mErr.ErrorOrNil()
}

// serviceDiscoveryDiff diffs the discovery blocks of a service.
func serviceDiscoveryDiff(old, new *ServiceDiscovery, contextual bool) *ObjectDiff {
	if old.Equal(new) {
		return nil
	}
	o, n := old, new
	if o == nil {
		o = &ServiceDiscovery{}
	}
	if n == nil {
		n = &ServiceDiscovery{}
	}
	diff := primitiveObjectDiff(o, n, nil, "Discovery", contextual)
	if diff == nil {
		diff = &ObjectDiff{Type: DiffTypeNone, Name: "Discovery"}
	}
	if f := fieldDiff(strings.Join(o.Failover, ","), strings.Join(n.Failover, ","), "Failover", contextual); f != nil {
		diff.Fields = append(diff.Fields, f)
		if diff.Type == DiffTypeNone {
			diff.Type = DiffTypeEdited
		}
	}
	switch {
	case old == nil:
		diff.Type = DiffTypeAdded
	case new == nil:
		diff.Type = DiffTypeDeleted
	}
	return diff
}

// EffectiveDiscovery returns the discovery settings that apply to the given
// registrations of one service. Registrations of the same service normally
// carry the same settings; if they differ (e.g. several jobs register the same
// name, or during a job update), the settings of the most recently modified
// registration win.
func EffectiveDiscovery(regs []*ServiceRegistration) *ServiceDiscovery {
	var newest *ServiceRegistration
	for _, reg := range regs {
		if reg.Discovery == nil {
			continue
		}
		if newest == nil || reg.ModifyIndex > newest.ModifyIndex {
			newest = reg
		}
	}
	if newest == nil {
		return nil
	}
	return newest.Discovery
}
