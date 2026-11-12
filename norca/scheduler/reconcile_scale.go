// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import "github.com/bacanoo-io/norca/norca/norca/structs"

// Scaling and the deployment of a task group.
//
// Scaling sets the count on the version of the job that is stored
// (state.ScaleJob), so the deployment of that version stays the deployment of
// the group, whatever its state: running, paused, failed. A deployment is the
// rollout of a version over the allocations the group had when it started.
// The allocations a scale up adds later are not its work:
//
//   - they are placed without the deployment (no deployment ID), so they do
//     not count as placed or healthy allocations of it;
//   - the deployment does not wait for them: it is complete when its own
//     allocations are, also while an added one waits for capacity. Without
//     this an allocation that can never be placed would keep the deployment
//     running until its progress deadline failed it, and an auto_revert would
//     take back a rollout that went well;
//   - a deployment that failed does not hold them. Placements are not made
//     for a group whose deployment failed (the version is not to spread),
//     but the count that was asked for by scaling is: without it a group
//     whose rollout failed could not be scaled up until a new version is
//     registered.
//
// A paused deployment, and one that waits for its turn in a multiregion
// rollout, holds them like every other placement, until it goes on.
//
// How many allocations scaling added is recorded on the deployment when the
// count is changed (DeploymentState.ScaleAdded, state.ScaleJob). Which ones
// they are is read from their names: the allocations of a group are named by
// an index below its count, new ones take the free indexes and a scale down
// stops the highest ones, so the added allocations are those with the
// highest indexes of the group. Replacements (of lost and of failed
// allocations) keep the name, and with it the place in the deployment, of
// the allocation they replace.

// markOutsideDeployment marks the placements of new allocations that a scale
// up added to a group after its deployment started: those whose index is
// first or above.
func markOutsideDeployment(jobID string, place []allocPlaceResult, first int) {
	for i := range place {
		p := &place[i]
		if p.previousAlloc != nil || p.reschedule || p.lost || p.canary {
			continue
		}
		if int(structs.AllocIndexFromName(p.name, jobID, p.taskGroup.Name)) >= first {
			p.outsideDeployment = true
		}
	}
}

// insideDeployment returns the number of placements that are the work of the
// group's deployment.
func insideDeployment(place []allocPlaceResult) int {
	n := 0
	for _, p := range place {
		if !p.outsideDeployment {
			n++
		}
	}
	return n
}

// outsideDeployment returns the placements, from the index first on, that a
// scale up added to the group after its deployment started.
func outsideDeployment(place []allocPlaceResult, first int) []allocPlaceResult {
	var res []allocPlaceResult
	for i := first; i < len(place); i++ {
		if place[i].outsideDeployment {
			res = append(res, place[i])
		}
	}
	return res
}

// placementDeploymentID returns the deployment a placed allocation is part
// of: none for an allocation that a scale up added to the group after its
// deployment started.
func placementDeploymentID(p placementResult, deploymentID string) string {
	if p.OutsideDeployment() {
		return ""
	}
	return deploymentID
}
