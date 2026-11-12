// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"sort"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
)

// The scale down of node groups. Only the scale_down block of a group lowers
// its scale; the checks of its policy only raise it (a check that would lower
// it is ignored, see evaluate). The decision is a number, spec.replicas lowered
// by one: which node goes, and whether one may go at all, is the operator's
// choice. A group is lowered when, for unneeded_for without a break:
//
//   - no blocked work could use it, and none was given to it in this look;
//   - it has all its nodes (none joining, none being removed) and is above min;
//   - its allocated share, without one node of the group, is below
//     utilization_below, and the free capacity without that node still covers
//     the headroom and the reserve for the loss of a datacenter (reserve.go);
//   - delay_after_add has passed since the scale was last raised, and the
//     group is not in its cooldown;
//   - the operator does not veto it: the condition ScaleDownBlocked, until
//     its retryAfter.
//
// When the operator vetoes a scale down that was already decided (no node of
// the group may go), the scale is set back to the nodes the group has, so the
// two do not pull the number up and down. One group of a pool at a time is
// lowered, by one node: the one furthest above its share of the pool
// (scaledown_balance.go), or the unneeded group itself, among several the one
// with the most nodes.

const (
	checkScaleDown     = "scale-down"
	checkScaleDownVeto = "scale-down-veto"

	// condScaleDownBlocked is the condition the operator sets when no node of
	// the group may go; retryAfter is when the autoscaler may try again.
	condScaleDownBlocked = "ScaleDownBlocked"

	defaultUnneededFor      = 10 * time.Minute
	defaultDelayAfterAdd    = 10 * time.Minute
	defaultUtilizationBelow = 40.0
)

// scaleDownSpec is the scale_down block of a group. Without the block the
// group is never lowered by the autoscaler.
type scaleDownSpec struct {
	enabled          bool
	unneededFor      time.Duration
	delayAfterAdd    time.Duration
	utilizationBelow float64
}

// scaleDownOf reads the scale_down block of the spec of a group.
func scaleDownOf(spec map[string]any) (scaleDownSpec, error) {
	block, ok := asMap(spec["policy"])["scale_down"].(map[string]any)
	if !ok {
		return scaleDownSpec{}, nil
	}
	s := scaleDownSpec{
		enabled:          true,
		unneededFor:      defaultUnneededFor,
		delayAfterAdd:    defaultDelayAfterAdd,
		utilizationBelow: defaultUtilizationBelow,
	}
	for key, dst := range map[string]*time.Duration{"unneeded_for": &s.unneededFor, "delay_after_add": &s.delayAfterAdd} {
		if raw := asString(block[key]); raw != "" {
			d, err := parseNodeDuration(raw)
			if err != nil {
				return scaleDownSpec{}, fmt.Errorf("scale_down.%s: %w", key, err)
			}
			*dst = d
		}
	}
	if v, ok := block["utilization_below"].(float64); ok {
		s.utilizationBelow = v
	}
	return s, nil
}

// scaleDownVeto reads the condition ScaleDownBlocked of the status: whether
// it is set, until when, and why. A veto without a retryAfter time holds
// until the operator clears it.
func scaleDownVeto(status map[string]any) (blocked bool, until time.Time, message string) {
	list, _ := status["conditions"].([]any)
	for _, raw := range list {
		c := asMap(raw)
		if asString(c["type"]) != condScaleDownBlocked || asString(c["status"]) != "True" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, asString(c["retryAfter"])); err == nil {
			until = t
		}
		return true, until, asString(c["message"])
	}
	return false, time.Time{}, ""
}

// vetoed reports whether the operator holds the scale down of the group now.
func (g *demandGroup) vetoed(now time.Time) bool {
	return g.veto && (g.vetoUntil.IsZero() || now.Before(g.vetoUntil))
}

// lastScaleUp returns the time of the last raise of the scale of the target,
// whoever made it.
func (t *targetState) lastScaleUp() time.Time {
	for _, e := range t.events {
		if e.Count == nil || e.Error {
			continue
		}
		if *e.Count > e.PreviousCount {
			return time.Unix(0, e.Time)
		}
	}
	return time.Time{}
}

// unneeded tells whether one node of the group could go now, and why; the
// reason says what failed when it can't.
func (d *demandRun) unneeded(g *demandGroup) (string, bool) {
	r, now := d.r, d.now
	if reason, ok := d.mayGo(g); !ok {
		return reason, false
	}

	nodes, share, ok := r.groupUsage(g.policy, now)
	if !ok || nodes < 1 {
		return "the cluster gauges can't be read", false
	}
	used := share * nodes // in nodes
	without := 100.0
	switch {
	case nodes > 1:
		without = used / (nodes - 1) * 100
	case used == 0:
		without = 0
	}
	if without >= g.down.utilizationBelow {
		return "", false
	}
	if g.keepsAhead() {
		wanted := g.aheadWanted(nodes - 1)
		if free := nodes - used - 1; free < wanted {
			return "", false
		}
	}
	return fmt.Sprintf("allocated %.0f%% of %.0f nodes, %.0f%% without one node (below %.0f%%)",
		share*100, nodes, without, g.down.utilizationBelow), true
}

// scaleDown lowers the scale of the groups that have been unneeded for long
// enough, one group of a pool at a time, and sets the scale back where the
// operator vetoed a scale down.
func (d *demandRun) scaleDown() {
	r, now := d.r, d.now

	// A pool where a node is being removed waits for it.
	removing := map[string]bool{}
	for _, g := range d.groups {
		if g.spec < g.ready {
			removing[g.pool] = true
		}
	}

	byPool := map[string][]*demandGroup{}
	reasons := map[*demandGroup]string{}
	for _, g := range d.groups {
		if g.veto && g.statusReplicas && g.spec < g.ready {
			reason := "the operator vetoed the scale down: " + g.vetoMsg
			if err := r.scaleGroup(g, g.ready, sdk.ScaleDirectionUp, reason, checkScaleDownVeto); err != nil {
				r.logger.Warn("failed to set the scale of a node group back", "group", g.name, "error", err)
			}
			continue
		}
		// Above the maximum of a window of spec.schedule: lowered a node at a
		// time, whatever its use and its scale_down block, but not while
		// nodes join or go, nor against a veto of the operator.
		if g.spec > g.max {
			delete(r.unneededSince, g.name)
			if g.pending > 0 || g.spec != g.ready || g.vetoed(now) {
				continue
			}
			desired := g.spec - 1
			if r.lastDown[g.name] == desired && g.policy.Target["dry-run"] == "true" {
				continue
			}
			reason := fmt.Sprintf("maximum %d (%s) [%d ready, spec %d]", g.max, g.windows, g.ready, g.spec)
			if err := r.scaleGroup(g, desired, sdk.ScaleDirectionDown, reason, checkSchedule); err != nil {
				r.logger.Warn("failed to lower a node group to the maximum of its window", "group", g.name, "error", err)
				continue
			}
			r.lastDown[g.name] = desired
			continue
		}
		reason, ok := d.unneeded(g)
		if !ok {
			delete(r.unneededSince, g.name)
			delete(r.lastDown, g.name)
			continue
		}
		since, seen := r.unneededSince[g.name]
		if !seen {
			r.unneededSince[g.name] = now
			continue
		}
		if now.Sub(since) < g.down.unneededFor {
			continue
		}
		reasons[g] = fmt.Sprintf("%s for %s", reason, now.Sub(since).Round(time.Second))
		byPool[g.pool] = append(byPool[g.pool], g)
	}

	pools := make([]string, 0, len(byPool))
	for pool := range byPool {
		pools = append(pools, pool)
	}
	sort.Strings(pools)
	for _, pool := range pools {
		if removing[pool] {
			continue
		}
		list := byPool[pool]
		sort.SliceStable(list, func(i, j int) bool { return list[i].ready > list[j].ready })
		g, reason := list[0], reasons[list[0]]

		// The pool has a node too many; the one that goes is of the group
		// furthest above its share (scaledown_balance.go).
		isForBalance := false
		if d.balanced(pool) {
			if over := d.overShare(pool, g); over != nil {
				reason = fmt.Sprintf("to balance the pool %s: %s is unneeded (%s) and %s has %d of the pool's nodes",
					pool, g.name, reason, over.name, over.ready)
				g, isForBalance = over, true
			}
		}

		desired := g.spec - 1
		if r.lastDown[g.name] == desired && g.policy.Target["dry-run"] == "true" {
			continue
		}
		if err := r.scaleGroup(g, desired, sdk.ScaleDirectionDown, reason, checkScaleDown); err != nil {
			r.logger.Warn("failed to lower the scale of a node group", "group", g.name, "error", err)
			continue
		}
		r.lastDown[g.name] = desired
		if isForBalance {
			r.balanceLowered[pool] = balanceMark{group: g.name, at: now}
		}
	}
}
