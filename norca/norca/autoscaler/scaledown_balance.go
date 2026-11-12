// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"time"
)

// The balance of a pool in the scale down. A node group is unneeded when its
// own nodes are little used (scaledown.go). After a datacenter was lost and
// has come back, that is the group of the returned datacenter: its nodes are
// empty, because the work was placed again on nodes that were added to the
// other datacenters. Lowering it, as the unneeded group, would leave the
// pool as the loss made it: all the work in the datacenters that grew, and
// the returned one at its minimum.
//
// So when a group of a pool is unneeded, the pool has a node too many, but
// the node that goes is one of the group that is furthest above its share of
// the pool. Its allocations are placed again where there is room, which is
// the unneeded group, and the datacenters come back into balance a node at a
// time.

const (
	// balanceRetry is how long after a group was lowered for the balance
	// blocked work that raises it again counts as proof that its work could
	// not move, and balanceOffFor how long the pool then goes without it.
	balanceRetry  = 30 * time.Minute
	balanceOffFor = 6 * time.Hour
)

// balanceMark is the last lowering of a group of a pool for the balance.
type balanceMark struct {
	group string
	at    time.Time
}

// mayGo tells whether the scale of a group may be lowered at all now,
// whatever its use: the conditions of unneeded that are not the use of its
// nodes. The reason says what holds it when it may not.
func (d *demandRun) mayGo(g *demandGroup) (string, bool) {
	r, now := d.r, d.now
	switch {
	case !g.down.enabled:
		return "", false
	case g.raised || g.demanded:
		return "blocked work could use the group", false
	case g.spec <= g.min:
		return "at its minimum", false
	case g.pending > 0 || g.spec != g.ready:
		return "nodes are joining or being removed", false
	case g.vetoed(now):
		return "the operator holds the scale down: " + g.vetoMsg, false
	case r.cooling(g):
		return "in its cooldown", false
	}
	_, t, err := r.groupTarget(g)
	if err != nil {
		return "", false
	}
	if last := t.lastScaleUp(); !last.IsZero() && now.Sub(last) < g.down.delayAfterAdd {
		return "raised less than delay_after_add ago", false
	}
	return "", true
}

// overShare returns the group of the pool that may go and is furthest above
// its share of the pool's ready nodes, by at least one node and by more than
// unneeded, the group that is unneeded; nil when unneeded is that group
// itself or no group is. The shares are the weights of the groups.
func (d *demandRun) overShare(pool string, unneeded *demandGroup) *demandGroup {
	var ready, weights int64
	for _, g := range d.groups {
		if g.pool == pool {
			ready += g.ready
			weights += g.weight
		}
	}
	if weights == 0 {
		return nil
	}
	excess := func(g *demandGroup) float64 {
		return float64(g.ready) - float64(ready)*float64(g.weight)/float64(weights)
	}

	var over *demandGroup
	floor, most := excess(unneeded), 0.0
	for _, g := range d.groups {
		if g.pool != pool || g == unneeded {
			continue
		}
		e := excess(g)
		isOver := e >= 1 && e > floor
		if !isOver || (over != nil && e <= most) {
			continue
		}
		if _, ok := d.mayGo(g); !ok {
			continue
		}
		over, most = g, e
	}
	return over
}

// balanced reports whether the scale down of a pool keeps its balance: its
// setting, unless the balance was switched off for a while because the work
// of a group that was lowered for it could not move (noteRaised).
func (d *demandRun) balanced(pool string) bool {
	if !d.cfg.pool(pool).balance() {
		return false
	}
	return !d.now.Before(d.r.balanceOffUntil[pool])
}

// noteRaised is told when blocked work raises a group. A group that was
// lowered for the balance a short while ago is being raised for the work it
// held: the work could not move to the unneeded group (the jobs are bound to
// its datacenter, most likely). The balance of the pool is switched off for
// balanceOffFor, so the pool is not lowered and raised in turns; the
// unneeded group itself goes then, as without the balance.
func (d *demandRun) noteRaised(g *demandGroup) string {
	r := d.r
	mark, ok := r.balanceLowered[g.pool]
	if !ok || mark.group != g.name || d.now.Sub(mark.at) > balanceRetry {
		return ""
	}
	delete(r.balanceLowered, g.pool)
	r.balanceOffUntil[g.pool] = d.now.Add(balanceOffFor)
	return fmt.Sprintf("the node group %s was lowered to balance the pool %s and is raised again for blocked work: "+
		"its work could not move, so the pool is lowered without the balance for %s", g.name, g.pool, balanceOffFor)
}
