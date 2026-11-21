// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"fmt"
	"slices"

	log "github.com/hashicorp/go-hclog"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// AllocAffinityViolations returns the placements of a plan that a required
// colocate or separate rule of their task group does not allow on the state,
// one line each (review finding SCH-6).
//
// The rules are between jobs, and the scheduler checks them on the snapshot
// of its evaluation: two evaluations of jobs that separate each other, run in
// parallel, both see the other's domain empty. The plan applier calls this on
// the state that holds every plan applied before, as it checks the quota. The
// placements are checked in the order of the nodes and of the plan, each
// with those before it, the stops and the preemptions of the plan, as the
// scheduler built it.
func AllocAffinityViolations(state State, plan *structs.Plan, logger log.Logger) ([]string, error) {
	if plan == nil || plan.Job == nil || len(plan.NodeAllocation) == 0 || !requiresAllocAffinity(plan.Job) {
		return nil, nil
	}

	partial := &structs.Plan{
		Job:             plan.Job,
		NodeUpdate:      plan.NodeUpdate,
		NodePreemptions: plan.NodePreemptions,
		NodeAllocation:  make(map[string][]*structs.Allocation, len(plan.NodeAllocation)),
	}
	ctx := NewEvalContext(nil, state, partial, logger)
	tracker := newAllocAffinityTracker(ctx)
	tracker.SetJob(plan.Job)

	nodeIDs := make([]string, 0, len(plan.NodeAllocation))
	for nodeID := range plan.NodeAllocation {
		nodeIDs = append(nodeIDs, nodeID)
	}
	slices.Sort(nodeIDs)

	var out []string
	for _, nodeID := range nodeIDs {
		node, err := state.NodeByID(nil, nodeID)
		if err != nil {
			return nil, err
		}
		for _, alloc := range plan.NodeAllocation[nodeID] {
			if node != nil && alloc != nil {
				reason, err := allocAffinityRefusal(state, tracker, plan.Job, node, alloc)
				if err != nil {
					return nil, err
				}
				if reason != "" {
					out = append(out, fmt.Sprintf("%s on node %s: %s", alloc.Name, nodeID, reason))
				}
			}
			partial.NodeAllocation[nodeID] = append(partial.NodeAllocation[nodeID], alloc)
		}
	}
	return out, nil
}

// allocAffinityRefusal returns why a required rule does not allow a new
// placement on the node, "" when it does or when the allocation is not new
// (an in-place update stays where it is).
func allocAffinityRefusal(state State, tracker *allocAffinityTracker, job *structs.Job,
	node *structs.Node, alloc *structs.Allocation) (string, error) {
	existing, err := state.AllocByID(nil, alloc.ID)
	if err != nil || existing != nil {
		return "", err
	}
	tg := job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil || !tg.HasAllocAffinities() {
		return "", nil
	}
	tracker.SetTaskGroup(tg)
	if !tracker.hasRequired() {
		return "", nil
	}
	if ok, reason := tracker.satisfiesRequired(node); !ok {
		return reason, nil
	}
	return "", nil
}

// requiresAllocAffinity reports whether a task group of the job has a required
// colocate or separate rule.
func requiresAllocAffinity(job *structs.Job) bool {
	for _, tg := range job.TaskGroups {
		for _, rules := range [][]*structs.AllocAffinity{tg.Colocate, tg.Separate} {
			for _, r := range rules {
				if r != nil && r.Required {
					return true
				}
			}
		}
	}
	return false
}
