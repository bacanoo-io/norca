// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"time"
)

// The health of a node group, as the blocked work trigger sees it. Work that
// cannot be placed is given to node groups that can take it; a group whose
// machines cannot be made, or do not come to serve, would keep the work
// waiting for nodes that never join while a healthy group of another
// datacenter could have taken it. That is what happens to a datacenter that
// is lost: its group has the fewest nodes left and looks like the best place
// to grow.

const (
	// The conditions the operator of the node groups sets in the status.
	condProviderAvailable = "ProviderAvailable"
	condProvisioned       = "Provisioned"
	condJoinFailing       = "JoinFailing"

	// reasonNoBootstrap is the reason of a Provisioned condition that is
	// False because the operator has nothing to start a node with.
	reasonNoBootstrap = "NoBootstrap"

	// unreadyAfter is how long fewer than half of the nodes of a group must
	// have been ready before the group counts as degraded: shorter than the
	// time a machine takes to join, longer than a node restart.
	unreadyAfter = 2 * time.Minute
)

// groupCondition returns the status, reason and message of a condition of a
// node group; the status is "" when the condition is not set.
func groupCondition(status map[string]any, typ string) (condStatus, reason, message string) {
	list, _ := status["conditions"].([]any)
	for _, raw := range list {
		c := asMap(raw)
		if asString(c["type"]) == typ {
			return asString(c["status"]), asString(c["reason"]), asString(c["message"])
		}
	}
	return "", "", ""
}

// unableReason says why no machine can be made for a node group, from the
// conditions of its operator; "" when nothing says so. Such a group is never
// given blocked work: raising its scale would change nothing.
func unableReason(status map[string]any) string {
	if st, reason, msg := groupCondition(status, condProviderAvailable); st == "False" {
		return describeCondition("its provider is not available", reason, msg)
	}
	if st, reason, msg := groupCondition(status, condJoinFailing); st == "True" {
		return describeCondition("its nodes fail to join", reason, msg)
	}
	if st, reason, msg := groupCondition(status, condProvisioned); st == "False" && reason == reasonNoBootstrap {
		return describeCondition("its nodes cannot be bootstrapped", reason, msg)
	}
	return ""
}

func describeCondition(what, reason, message string) string {
	switch {
	case message != "":
		return fmt.Sprintf("%s (%s)", what, message)
	case reason != "":
		return fmt.Sprintf("%s (%s)", what, reason)
	}
	return what
}

// degradedReason says whether the nodes of a group are missing: of the nodes
// the group should have (its scale less those still joining), fewer than
// half have been ready for unreadyAfter. A datacenter that is cut off looks
// like this before any condition tells. Such a group is given blocked work
// only when no healthy group can take it: one node that died in the only
// group there is must still be replaced.
//
// since is when the group was first seen so, by group name; it is kept
// between the looks of the demand loop.
func degradedReason(g *demandGroup, since map[string]time.Time, now time.Time) string {
	expected := g.spec - g.pending
	if expected <= 0 || g.ready*2 >= expected {
		delete(since, g.name)
		return ""
	}
	first, ok := since[g.name]
	if !ok {
		since[g.name] = now
		return ""
	}
	if now.Sub(first) < unreadyAfter {
		return ""
	}
	return fmt.Sprintf("%d of its %d nodes are ready, for %s", g.ready, expected, now.Sub(first).Round(time.Second))
}
