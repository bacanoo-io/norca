// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"fmt"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// advice is the state of the advice mode between two looks at the blocked
// work: what was told last, so that an event is raised when the picture
// changes and not at every look.
type advice struct {
	nodes    map[[2]string]int // advised nodes by {pool, datacenter}
	tooLarge map[string]bool   // the allocations reported as too large
	unverif  map[string]bool   // the constraints reported as unverifiable
}

func newAdvice() *advice {
	return &advice{nodes: map[[2]string]int{}, tooLarge: map[string]bool{}, unverif: map[string]bool{}}
}

// advise runs the read-only advice mode until ctx is canceled: it packs the
// work that cannot be placed into nodes of the configured size and raises an
// event with the number of nodes that would be added. It changes nothing.
func (a *Autoscaler) advise(ctx context.Context, cfg *Config) {
	st := newAdvice()
	ticker := time.NewTicker(cfg.AdviceInterval)
	defer ticker.Stop()
	for {
		a.adviseOnce(cfg, st)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// adviseOnce is one look at the blocked work.
func (a *Autoscaler) adviseOnce(cfg *Config, st *advice) {
	demand := a.deps.BlockedDemand()
	now := a.now().UnixNano()
	publish := func(typ string, ev *structs.ScalingDecisionEvent) {
		if a.deps.PublishEvent != nil {
			ev.Time = now
			a.deps.PublishEvent(typ, ev)
		}
	}

	stillTooLarge := map[string]bool{}
	stillUnverif := map[string]bool{}
	for _, g := range cfg.Advice {
		// Only the part of the work a new node of this group could take.
		var usable []Unplaced
		for _, u := range demand[[2]string{g.Datacenter, g.Pool}] {
			if u.Unverifiable != "" {
				id := fmt.Sprintf("%s/%s/%s/%s/%s", g.Pool, g.Datacenter, u.Namespace, u.Job, u.Group)
				stillUnverif[id] = true
				if !st.unverif[id] {
					st.unverif[id] = true
					publish(structs.TypeScalingNodeUnverifiable, &structs.ScalingDecisionEvent{
						Namespace: u.Namespace,
						JobID:     u.Job,
						Group:     u.Group,
						Direction: "up",
						Reason:    "the constraint " + u.Unverifiable + " cannot be checked against a node that does not exist yet, so no node is advised for it",
					})
				}
				continue
			}
			if r, ok := u.Restrict(g); ok {
				usable = append(usable, r)
			}
		}
		res := PackNodes(usable, g.Size)
		key := [2]string{g.Pool, g.Datacenter}

		for _, u := range res.TooLarge {
			id := fmt.Sprintf("%s/%s/%s/%s/%s", g.Pool, g.Datacenter, u.Namespace, u.Job, u.Group)
			stillTooLarge[id] = true
			if st.tooLarge[id] {
				continue
			}
			st.tooLarge[id] = true
			publish(structs.TypeScalingNodeAllocationTooLarge, &structs.ScalingDecisionEvent{
				Namespace: u.Namespace,
				JobID:     u.Job,
				Group:     u.Group,
				Direction: "up",
				Reason: fmt.Sprintf("an allocation of %.0f MHz, %.0f MB memory, %.0f MB disk does not fit an empty node of %s (%.0f MHz, %.0f MB, %.0f MB)",
					u.CPU, u.Memory, u.Disk, g.Name, g.Size.CPU, g.Size.Memory, g.Size.Disk),
			})
		}

		if prev := st.nodes[key]; prev != res.Nodes {
			st.nodes[key] = res.Nodes
			direction := "up"
			reason := fmt.Sprintf("would add %d nodes to %s in %s for the work that cannot be placed", res.Nodes, g.Pool, g.Datacenter)
			if res.Nodes == 0 {
				direction = "down"
				reason = fmt.Sprintf("no work is waiting for nodes in %s in %s any more", g.Pool, g.Datacenter)
			}
			publish(structs.TypeScalingNodeAdvice, &structs.ScalingDecisionEvent{
				Group:         g.Pool + "/" + g.Datacenter,
				PreviousCount: int64(prev),
				Count:         int64(res.Nodes),
				DesiredCount:  int64(res.Nodes),
				Direction:     direction,
				Check:         g.Name,
				Reason:        reason,
				DryRun:        true,
			})
		}
	}
	for id := range st.tooLarge {
		if !stillTooLarge[id] {
			delete(st.tooLarge, id)
		}
	}
	for id := range st.unverif {
		if !stillUnverif[id] {
			delete(st.unverif, id)
		}
	}
}
