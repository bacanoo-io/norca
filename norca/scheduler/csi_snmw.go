// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import "github.com/bacanoo-io/norca/norca/norca/structs"

// A single-node-multi-writer volume keeps all its allocations on one node,
// the node of its claims (CSIVolume.ClaimedOnOtherNode). A claim is made when
// an allocation starts on its client, so the allocations of a group with a
// count above one were placed with no claim in sight, each on any node, and
// the second one's claim was refused on its client (review finding SCH-7).
// The allocations of the job that use such a volume and may not have claimed
// it yet pin it to their node too: those of the plan, and the live ones of
// the state that the plan does not stop.

// singleNodeVolumeNodes returns, by volume source, the node of the allocations
// of the job that use one of the volumes in the single-node-multi-writer mode.
func (c *CSIVolumeChecker) singleNodeVolumeNodes(volumes map[string]*structs.VolumeRequest) map[string]string {
	sources := make(map[string]bool)
	for _, req := range volumes {
		if !req.PerAlloc && structs.IsSingleNodeMultiWriterMode(req.AccessMode) {
			sources[req.Source] = true
		}
	}
	if len(sources) == 0 || c.ctx == nil {
		return nil
	}

	out := make(map[string]string)
	note := func(alloc *structs.Allocation, job *structs.Job, nodeID string) {
		if job == nil || nodeID == "" {
			return
		}
		tg := job.LookupTaskGroup(alloc.TaskGroup)
		if tg == nil {
			return
		}
		for _, v := range tg.Volumes {
			if v.Type == structs.VolumeTypeCSI && !v.PerAlloc && sources[v.Source] {
				if _, set := out[v.Source]; !set {
					out[v.Source] = nodeID
				}
			}
		}
	}

	stopping := make(map[string]bool)
	if plan := c.ctx.Plan(); plan != nil {
		for _, allocs := range plan.NodeUpdate {
			for _, alloc := range allocs {
				stopping[alloc.ID] = true
			}
		}
		for nodeID, allocs := range plan.NodeAllocation {
			for _, alloc := range allocs {
				if alloc.Namespace != c.namespace || alloc.JobID != c.jobID {
					continue
				}
				job := alloc.Job
				if job == nil {
					job = plan.Job
				}
				note(alloc, job, nodeID)
			}
		}
	}

	allocs, err := c.ctx.State().AllocsByJob(nil, c.namespace, c.jobID, false)
	if err != nil {
		return out
	}
	for _, alloc := range allocs {
		if alloc.TerminalStatus() || stopping[alloc.ID] || alloc.DesiredStatus != structs.AllocDesiredStatusRun {
			continue
		}
		note(alloc, alloc.Job, alloc.NodeID)
	}
	return out
}
