// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package discovery

import (
	"context"
	"slices"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Query is a lookup of the instances of a service.
type Query struct {
	// Namespace and Name of the service.
	Namespace string
	Name      string

	// Region of the service; empty for the region of the client.
	Region string

	// Datacenter, if set, limits the instances to one datacenter.
	Datacenter string

	// Tag, if set, limits the instances to those registered with it.
	Tag string
}

// Result is the answer to a lookup.
type Result struct {
	// Instances are the eligible instances, in topology order.
	Instances []*structs.ServiceRegistration

	// Region the instances come from; differs from the queried region after
	// a regional failover.
	Region string

	// Known reports whether the service has any registration in the queried
	// region, eligible or not.
	Known bool
}

// Resolver resolves lookups from a Cache: it keeps the healthy instances (and
// those of services publishing not ready instances), selects them by topology
// relative to the client, and fails over to the regions of the failover list
// of the service when the local region has none.
type Resolver struct {
	cache *Cache
	local func() structs.Topology
}

// NewResolver returns a resolver reading from cache. local returns the
// topology of the client.
func NewResolver(cache *Cache, local func() structs.Topology) *Resolver {
	return &Resolver{cache: cache, local: local}
}

// Resolve answers q.
func (r *Resolver) Resolve(ctx context.Context, q Query) (*Result, error) {
	local := r.local()
	region := q.Region
	if region == "" {
		region = local.Region
	}

	regs, discovery, err := r.cache.Get(ctx, Key{Region: region, Namespace: q.Namespace, Name: q.Name})
	if err != nil {
		return nil, err
	}

	result := &Result{Region: region, Known: len(regs) > 0}
	result.Instances = Eligible(regs, q.Tag, q.Datacenter)
	if region == local.Region {
		result.Instances = structs.SelectByTopology(result.Instances, local, discovery)
	}
	if len(result.Instances) > 0 || region != local.Region || discovery == nil {
		return result, nil
	}

	// regional failover, in the order of the list
	for _, failover := range discovery.Failover {
		if failover == local.Region {
			continue
		}
		regs, _, err := r.cache.Get(ctx, Key{Region: failover, Namespace: q.Namespace, Name: q.Name})
		if err != nil {
			continue
		}
		if instances := Eligible(regs, q.Tag, q.Datacenter); len(instances) > 0 {
			return &Result{Instances: instances, Region: failover, Known: true}, nil
		}
	}
	return result, nil
}

// Eligible returns the instances of regs that are discoverable and carry tag
// and belong to datacenter, if those are set.
func Eligible(regs []*structs.ServiceRegistration, tag, datacenter string) []*structs.ServiceRegistration {
	out := make([]*structs.ServiceRegistration, 0, len(regs))
	for _, reg := range regs {
		if !reg.Discoverable() {
			continue
		}
		if tag != "" && !slices.Contains(reg.Tags, tag) {
			continue
		}
		if datacenter != "" && reg.Datacenter != datacenter {
			continue
		}
		out = append(out, reg)
	}
	return out
}
