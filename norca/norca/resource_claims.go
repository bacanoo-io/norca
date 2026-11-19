// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"sync"
)

// The claims registry says who is writing a resource now. Releases and
// tenants record what they own in their status, and a release does it only
// after it wrote its resources: until then nothing in the state tells a
// tenant or another release that the resource is taken (review finding
// ATM-4). Both run on the leader, so the leader keeps the claims in memory:
// a release operation claims the resources of its pack before it checks
// their owners and keeps the claims until it recorded what it owns; a pass
// over a tenant claims the resources the tenant wants before it checks
// their owners and keeps them until its status is written. A resource
// claimed by somebody else is a conflict.
//
// Whoever got a claim reads the owners from the state again after it: an
// owner that gave its claims up recorded what it owns before. Nobody
// records ownership of a resource without holding its claim, so the owners
// read then stay true until the claims are given up.
//
// The claims are the leader's: a server that is no longer the leader keeps
// the claims of its running operations, and the new leader starts without
// any.

// Kinds of claimants.
const (
	claimRelease = "release"
	claimTenant  = "tenant"
)

// resourceClaimant is one release operation or one pass over a tenant. Two
// operations of one owner are two claimants.
type resourceClaimant struct {
	id   uint64
	kind string
	name string
}

// String names the claimant in messages: release "ns/name" or tenant
// "name".
func (c resourceClaimant) String() string {
	return fmt.Sprintf("%s %q", c.kind, c.name)
}

// resourceClaims maps the keys of resources (ReleaseResourceRef.Key, which
// tenantResourceRef.key matches) to their claimant.
type resourceClaims struct {
	mu     sync.Mutex
	lastID uint64
	held   map[string]resourceClaimant
}

func newResourceClaims() *resourceClaims {
	return &resourceClaims{held: make(map[string]resourceClaimant)}
}

// claimant returns a new claimant of the kind and name.
func (c *resourceClaims) claimant(kind, name string) resourceClaimant {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastID++
	return resourceClaimant{id: c.lastID, kind: kind, name: name}
}

// claim claims for who every key that nobody else claims, and returns the
// others with their claimant.
func (c *resourceClaims) claim(who resourceClaimant, keys []string) map[string]resourceClaimant {
	c.mu.Lock()
	defer c.mu.Unlock()
	var others map[string]resourceClaimant
	for _, key := range keys {
		if holder, ok := c.held[key]; ok && holder.id != who.id {
			if others == nil {
				others = make(map[string]resourceClaimant)
			}
			others[key] = holder
			continue
		}
		c.held[key] = who
	}
	return others
}

// release gives up every claim of who.
func (c *resourceClaims) release(who resourceClaimant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, holder := range c.held {
		if holder.id == who.id {
			delete(c.held, key)
		}
	}
}

// holder returns who claims the key.
func (c *resourceClaims) holder(key string) (resourceClaimant, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	holder, ok := c.held[key]
	return holder, ok
}

// holders returns the claimants of those of the keys that are claimed.
func (c *resourceClaims) holders(keys []string) map[string]resourceClaimant {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out map[string]resourceClaimant
	for _, key := range keys {
		if holder, ok := c.held[key]; ok {
			if out == nil {
				out = make(map[string]resourceClaimant)
			}
			out[key] = holder
		}
	}
	return out
}

// holds reports whether who claims the key.
func (c *resourceClaims) holds(who resourceClaimant, key string) bool {
	holder, ok := c.holder(key)
	return ok && holder.id == who.id
}
