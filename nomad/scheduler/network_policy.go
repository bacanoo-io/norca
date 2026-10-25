// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// networkPolicyStateReader is the state store access the network policy
// checker needs. It is kept separate from the State interface so that
// states without network policies keep working: the checker then never
// filters.
type networkPolicyStateReader interface {
	NetworkPoliciesForGroup(ws memdb.WatchSet, namespace string, meta map[string]string) ([]*structs.NetworkPolicy, []*structs.AdminNetworkPolicy, error)
}

// NetworkPolicyChecker is a feasibility checker that filters out the nodes
// that cannot enforce the network policies with enforcement "required"
// selecting the task group: nodes without nftables support, and every node
// if the network mode of the group cannot enforce rules (e.g. host mode).
// Groups selected only by "best_effort" policies, or by no policy, are not
// affected.
//
// The result only depends on the node attributes, so it is cached per
// computed node class by the feasibility wrapper.
type NetworkPolicyChecker struct {
	ctx Context
	job *structs.Job

	// required is set when a required policy selects the task group.
	required bool
	mode     string
}

// NewNetworkPolicyChecker creates a network policy checker.
func NewNetworkPolicyChecker(ctx Context) *NetworkPolicyChecker {
	return &NetworkPolicyChecker{ctx: ctx}
}

// SetJob sets the job of the task groups to check.
func (c *NetworkPolicyChecker) SetJob(job *structs.Job) {
	c.job = job
}

// SetTaskGroup looks up the policies selecting the task group.
func (c *NetworkPolicyChecker) SetTaskGroup(tg *structs.TaskGroup) {
	c.required = false
	c.mode = structs.NetworkPolicyGroupMode(tg)
	if c.job == nil || tg == nil {
		return
	}
	reader, ok := c.ctx.State().(networkPolicyStateReader)
	if !ok {
		return
	}
	policies, admin, err := reader.NetworkPoliciesForGroup(nil, c.job.Namespace, structs.NetworkPolicyMeta(c.job, tg))
	if err != nil {
		c.ctx.Logger().Error("failed to look up network policies", "error", err)
		return
	}
	for _, p := range policies {
		if p.Required() {
			c.required = true
			return
		}
	}
	for _, p := range admin {
		if p.Required() {
			c.required = true
			return
		}
	}
}

// Feasible reports whether the node can enforce the required policies.
func (c *NetworkPolicyChecker) Feasible(node *structs.Node) bool {
	if !c.required {
		return true
	}
	ok, reason := structs.NetworkPolicyNodeCanEnforce(node, c.mode)
	if !ok {
		c.ctx.Metrics().FilterNode(node, reason)
	}
	return ok
}
