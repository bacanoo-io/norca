// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper/pointer"
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
	// the response, so a small zone does not receive the entire traffic. It is
	// the default of the default_overflow_fraction server setting, which the
	// servers fill in at job registration for discovery blocks that leave the
	// fraction unset.
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
	//
	// Nil means unset: the servers replace it with their default overflow
	// fraction (the default_overflow_fraction server setting) when the job
	// is registered, so stored jobs and service registrations carry a
	// concrete value. Jobs stored before the field became a pointer decode
	// with the value they were stored with.
	OverflowFraction *float64

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
	nd.OverflowFraction = pointer.Copy(d.OverflowFraction)
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
		pointer.Eq(d.OverflowFraction, o.OverflowFraction) &&
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
	if f := d.OverflowFraction; f != nil && (*f < 0 || *f > 1) {
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

// SelectByTopology orders the instances of one service by their topology
// relative to near, following the discovery settings d, and leaves out the
// tiers that are not needed. The tiers are (1) the zone of near, (2) its
// datacenter and (3) the rest of the region, depending on d.Prefer. The first
// tier is always returned; each next tier is added while the instances
// returned so far are fewer than d.MinLocal (at least one), or fewer than
// d.OverflowFraction of all instances. The order within a tier is kept.
//
// Instances must all belong to the region of near. With no settings, or with
// a preference of "none" or "region", instances are returned as they are.
func SelectByTopology(instances []*ServiceRegistration, near Topology, d *ServiceDiscovery) []*ServiceRegistration {
	if d == nil || len(instances) == 0 {
		return instances
	}

	var tiers [][]*ServiceRegistration
	switch d.Prefer {
	case DiscoveryPreferZone:
		var zone, dc, rest []*ServiceRegistration
		for _, inst := range instances {
			switch {
			case near.Zone != "" && inst.Zone == near.Zone && inst.Datacenter == near.Datacenter:
				zone = append(zone, inst)
			case inst.Datacenter == near.Datacenter:
				dc = append(dc, inst)
			default:
				rest = append(rest, inst)
			}
		}
		tiers = [][]*ServiceRegistration{zone, dc, rest}
	case DiscoveryPreferDatacenter:
		var dc, rest []*ServiceRegistration
		for _, inst := range instances {
			if inst.Datacenter == near.Datacenter {
				dc = append(dc, inst)
			} else {
				rest = append(rest, inst)
			}
		}
		tiers = [][]*ServiceRegistration{dc, rest}
	default:
		return instances
	}

	minLocal := d.MinLocal
	if minLocal < 1 {
		minLocal = 1
	}
	minFraction := d.EffectiveOverflowFraction() * float64(len(instances))

	out := make([]*ServiceRegistration, 0, len(instances))
	for i, tier := range tiers {
		if i > 0 && len(out) >= minLocal && float64(len(out)) >= minFraction {
			break
		}
		out = append(out, tier...)
	}
	return out
}

// EffectiveOverflowFraction returns the overflow fraction of d. An unset
// fraction, which the servers resolve at job registration, falls back to
// DefaultDiscoveryOverflowFraction.
func (d *ServiceDiscovery) EffectiveOverflowFraction() float64 {
	if d == nil || d.OverflowFraction == nil {
		return DefaultDiscoveryOverflowFraction
	}
	return *d.OverflowFraction
}

// SetDefaultOverflowFraction sets the overflow fraction of d to fraction if
// it is unset. An explicit value, including zero, is kept.
func (d *ServiceDiscovery) SetDefaultOverflowFraction(fraction float64) {
	if d != nil && d.OverflowFraction == nil {
		d.OverflowFraction = pointer.Of(fraction)
	}
}

// formatOverflowFraction formats an overflow fraction for job diffs; an
// unset fraction is empty.
func formatOverflowFraction(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'g', -1, 64)
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
	// primitiveObjectDiff skips pointer and slice fields
	for _, f := range []*FieldDiff{
		fieldDiff(strings.Join(o.Failover, ","), strings.Join(n.Failover, ","), "Failover", contextual),
		fieldDiff(formatOverflowFraction(o.OverflowFraction), formatOverflowFraction(n.OverflowFraction), "OverflowFraction", contextual),
	} {
		if f == nil {
			continue
		}
		diff.Fields = append(diff.Fields, f)
		if f.Type != DiffTypeNone && diff.Type == DiffTypeNone {
			diff.Type = DiffTypeEdited
		}
	}
	sort.Sort(FieldDiffs(diff.Fields))
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
