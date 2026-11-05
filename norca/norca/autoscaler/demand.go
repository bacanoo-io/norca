// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The blocked work trigger of node scaling. Work that cannot be placed for
// want of capacity (the blocked evaluations of the leader) is packed into
// virtual nodes of the node groups that could take it, and spec.replicas of
// those groups is raised by the number of nodes the work needs, less the
// nodes that were already asked for and have not joined yet. Which group of a
// pool gets a piece of the work is the expander's choice. The work is not
// held back by the cooldown of a group (the cooldown holds the ahead-of-time
// checks); the rate limit and the oscillation guard of the executor apply.

const (
	// DefaultNodeScalingInterval is how often the blocked work is looked at.
	DefaultNodeScalingInterval = 30 * time.Second

	ExpanderSpread   = "spread"
	ExpanderFill     = "fill"
	ExpanderPriority = "priority"
	ExpanderWeighted = "weighted"

	// maxDemandUnits bounds the allocations placed one by one in a cycle.
	maxDemandUnits = 5000

	checkBlockedDemand = "blocked-demand"
	checkHeadroom      = "headroom"
)

// NodeScalingConfig are the settings of node scaling that concern the groups
// of a pool together, or the whole region.
type NodeScalingConfig struct {
	// Expander chooses the group of a pool that grows for work that can go
	// to several: spread (default), fill, priority or weighted.
	Expander string

	// Interval is how often the blocked work is looked at.
	Interval time.Duration

	// Pools overrides the settings of a node pool.
	Pools map[string]*NodePoolScaling
}

// NodePoolScaling are the settings of one node pool.
type NodePoolScaling struct {
	Expander string
	// MaxNodes is the largest sum of spec.replicas over the groups of the
	// pool (0: no limit), MaxStep the nodes added to the pool by one
	// decision (0: no limit).
	MaxNodes int
	MaxStep  int
}

func validExpander(s string) bool {
	switch s {
	case "", ExpanderSpread, ExpanderFill, ExpanderPriority, ExpanderWeighted:
		return true
	}
	return false
}

// pool returns the settings of a pool with the defaults of the region.
func (c *NodeScalingConfig) pool(name string) NodePoolScaling {
	res := NodePoolScaling{Expander: ExpanderSpread}
	if c != nil {
		if c.Expander != "" {
			res.Expander = c.Expander
		}
		if p := c.Pools[name]; p != nil {
			if p.Expander != "" {
				res.Expander = p.Expander
			}
			res.MaxNodes, res.MaxStep = p.MaxNodes, p.MaxStep
		}
	}
	return res
}

func (c *NodeScalingConfig) interval() time.Duration {
	if c != nil && c.Interval > 0 {
		return c.Interval
	}
	return DefaultNodeScalingInterval
}

// demandGroup is a NodeGroup as the blocked work trigger sees it.
type demandGroup struct {
	policy *structs.ScalingPolicy
	name   string
	pool   string
	dc     string

	spec     int64 // spec.replicas
	ready    int64
	pending  int64 // asked for and not joined yet
	min, max int64
	priority int64
	weight   int64

	// spill lets work of a job that may run in any node pool ("all") grow
	// this group, whatever its pool is.
	spill bool
	class string // node_class

	size      NodeSize
	sizeKnown bool

	// The headroom: free capacity worth this many nodes of the group, or
	// this share of the nodes it has (the larger applies), and the free
	// capacity the cluster gauges show, in nodes.
	headNodes   int64
	headPercent float64
	free        float64
	freeNodes   float64 // ready, eligible nodes the free capacity is in
	freeKnown   bool

	// nodes are the virtual nodes of the work: the nodes already on their
	// way come first, empty, and a node is opened when the work does not
	// fit the ones there are. bucket is the work given to the group.
	nodes  []vnode
	bucket []Unplaced
	// projected is the number of nodes the group would have with the work.
	projected int64
}

// vnode is a virtual node: the room that is left on it, whether any work was
// put on it, and whether it holds work that can't share a node with its own
// kind (distinct_hosts, a static port).
type vnode struct {
	free      NodeSize
	used      bool
	exclusive bool
}

// openNodes opens a virtual node for each of the nodes on their way, once
// the size of a node is known: the work is placed on them first, so it is not
// asked for twice when it is split in another way than in the last look.
func (g *demandGroup) openNodes() {
	g.nodes = nil
	if !g.sizeKnown {
		return
	}
	for range g.pending {
		g.nodes = append(g.nodes, vnode{free: g.size})
	}
}

// fitIndex returns the virtual node the allocation fits, -1 if none.
func (g *demandGroup) fitIndex(u Unplaced) int {
	for i, n := range g.nodes {
		if u.Exclusive && n.exclusive {
			continue
		}
		if u.CPU <= n.free.CPU && u.Memory <= n.free.Memory && u.Disk <= n.free.Disk {
			return i
		}
	}
	return -1
}

// place puts the allocation on a virtual node, a new one when i is -1.
func (g *demandGroup) place(u Unplaced) {
	i := g.fitIndex(u)
	if i < 0 {
		g.nodes = append(g.nodes, vnode{free: g.size})
		i = len(g.nodes) - 1
	}
	n := &g.nodes[i]
	n.free = NodeSize{n.free.CPU - u.CPU, n.free.Memory - u.Memory, n.free.Disk - u.Disk}
	n.used = true
	n.exclusive = n.exclusive || u.Exclusive
	g.bucket = append(g.bucket, u)
	g.projected = g.spec + g.opened()
}

// opened is the number of nodes the work opened beyond those on their way.
func (g *demandGroup) opened() int64 {
	return max(int64(len(g.nodes))-g.pending, 0)
}

// pendingFree is the number of nodes on their way that no work was put on.
func (g *demandGroup) pendingFree() int64 {
	var n int64
	for i := int64(0); i < g.pending && i < int64(len(g.nodes)); i++ {
		if !g.nodes[i].used {
			n++
		}
	}
	return n
}

// readDemandGroups returns the enabled NodeGroups of the built-in engine.
func (r *runner) readDemandGroups(now time.Time) ([]*demandGroup, error) {
	store := r.deps.State()
	policies, _, err := nodeGroupPolicies(nil, store)
	if err != nil {
		return nil, err
	}
	var groups []*demandGroup
	for _, p := range policies {
		if !r.isBuiltinPolicy(p) {
			continue
		}
		name := p.Target[structs.ScalingTargetNodeGroup]
		obj, err := store.CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", name)
		if err != nil || obj == nil {
			continue
		}
		spec, status := obj.Spec, obj.Status
		g := &demandGroup{
			policy:   p,
			name:     name,
			pool:     asString(spec["pool"]),
			dc:       asString(spec["datacenter"]),
			spec:     asInt(spec["replicas"]),
			min:      asInt(spec["min"]),
			max:      asInt(spec["max"]),
			priority: asInt(spec["priority"]),
			weight:   max(asInt(spec["weight"]), 1),
		}
		g.spill, _ = spec["spill"].(bool)
		g.class = asString(spec["node_class"])

		// The nodes the group has: as the operator reports them, or, when
		// none does, the ready nodes of the cluster gauges.
		if v, ok := status["replicas"]; ok {
			g.ready = asInt(v)
		} else if n, ok := r.readyNodes(p, now); ok {
			g.ready = n
		} else {
			r.logger.Debug("node group left alone, its ready nodes are not known", "group", name)
			continue
		}
		if v, ok := status["joining"]; ok {
			g.pending = asInt(v)
		} else {
			g.pending = max(g.spec-g.ready, 0)
		}

		// The size of a node: what the operator reports, or what the spec
		// says by hand, less the reserved resources.
		if alloc := asMap(status["allocatable"]); asInt(alloc["cpu"]) > 0 && asInt(alloc["memory"]) > 0 {
			g.size = NodeSize{CPU: float64(asInt(alloc["cpu"])), Memory: float64(asInt(alloc["memory"])), Disk: float64(asInt(alloc["disk"]))}
			g.sizeKnown = true
		} else if node := asMap(spec["node"]); node != nil {
			if capc := asMap(node["capacity"]); asInt(capc["cpu"]) > 0 && asInt(capc["memory"]) > 0 {
				res := asMap(node["reserved"])
				g.size = NodeSize{
					CPU:    float64(asInt(capc["cpu"]) - asInt(res["cpu"])),
					Memory: float64(asInt(capc["memory"]) - asInt(res["memory"])),
					Disk:   float64(asInt(capc["disk"]) - asInt(res["disk"])),
				}
				g.sizeKnown = g.size.CPU > 0 && g.size.Memory > 0
			}
		}
		g.headNodes = asInt(spec["headroom_nodes"])
		if v, ok := spec["headroom_percent"].(float64); ok {
			g.headPercent = v
		}
		if g.headNodes > 0 || g.headPercent > 0 {
			g.freeNodes, g.free, g.freeKnown = r.freeCapacity(p, now)
		}
		g.projected = g.spec
		g.openNodes()
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].name < groups[j].name })
	return groups, nil
}

// readyNodes reads the ready nodes of the group's datacenter, pool and class
// from the cluster gauges. When they can't be read the last count that could
// be is used; it is false when there is none, and the group is left alone:
// without the nodes the group has, the nodes on their way can't be told, and
// the same work would be asked for again.
func (r *runner) readyNodes(p *structs.ScalingPolicy, now time.Time) (int64, bool) {
	name := p.Target[structs.ScalingTargetNodeGroup]
	if r.deps.GaugeHistory != nil {
		labels := map[string]string{
			"dc":    p.Target[TargetConfigKeyDatacenter],
			"pool":  p.Target[TargetConfigKeyPool],
			"class": p.Target[TargetConfigKeyNodeClass],
		}
		avg, _, samples, err := r.deps.GaugeHistory(metricsstore.MetricClusterNodesReady, labels, now.Add(-clusterQueryWindow), now)
		if err == nil && samples > 0 {
			n := int64(avg + 0.5)
			r.readySeen[name] = n
			return n, true
		}
	}
	n, ok := r.readySeen[name]
	return n, ok
}

// freeCapacity reads the free capacity of the group from the cluster gauges:
// the ready, eligible nodes of its datacenter, pool and class, and how many
// of them are free, in nodes (the scarcer of CPU and memory, as the share of
// the capacity that is not allocated). It is false when the gauges can't be
// read.
func (r *runner) freeCapacity(p *structs.ScalingPolicy, now time.Time) (ready, free float64, ok bool) {
	if r.deps.GaugeHistory == nil {
		return 0, 0, false
	}
	labels := map[string]string{
		"dc":    p.Target[TargetConfigKeyDatacenter],
		"pool":  p.Target[TargetConfigKeyPool],
		"class": p.Target[TargetConfigKeyNodeClass],
	}
	read := func(metric string) (float64, bool) {
		avg, _, samples, err := r.deps.GaugeHistory(metric, labels, now.Add(-clusterQueryWindow), now)
		return avg, err == nil && samples > 0
	}
	nodes, ok1 := read(metricsstore.MetricClusterNodesReady)
	cpu, ok2 := read(metricsstore.MetricClusterCPUAllocated)
	memory, ok3 := read(metricsstore.MetricClusterMemoryAllocated)
	if !ok1 || !ok2 || !ok3 {
		return 0, 0, false
	}
	allocated := max(cpu, memory) / 100
	return nodes, nodes * (1 - min(allocated, 1)), true
}

// demandItem is the blocked work of one task group, with the datacenters it
// could go to.
type demandItem struct {
	Unplaced
	pool string
	dcs  []string
}

// collectDemandItems merges the demand by datacenter and pool into one item
// per task group: the same work is reported in each datacenter it could use,
// but it is placed once.
func collectDemandItems(demand map[[2]string][]Unplaced) []*demandItem {
	byKey := map[string]*demandItem{}
	var keys []string
	for k, list := range demand {
		for _, u := range list {
			id := strings.Join([]string{k[1], u.Namespace, u.Job, u.Group}, "\x00")
			item := byKey[id]
			if item == nil {
				item = &demandItem{Unplaced: u, pool: k[1]}
				byKey[id] = item
				keys = append(keys, id)
			}
			if !slices.Contains(item.dcs, k[0]) {
				item.dcs = append(item.dcs, k[0])
			}
		}
	}
	slices.Sort(keys)
	items := make([]*demandItem, 0, len(keys))
	for _, id := range keys {
		slices.Sort(byKey[id].dcs)
		items = append(items, byKey[id])
	}
	return items
}

// demandRun is one look at the blocked work.
type demandRun struct {
	r      *runner
	now    time.Time
	groups []*demandGroup
	cfg    *NodeScalingConfig
}

// expand picks the group that grows for one allocation of the work.
func (d *demandRun) expand(pool string, candidates []*demandGroup) *demandGroup {
	best := candidates[0]
	var better func(a, b *demandGroup) bool
	switch d.cfg.pool(pool).Expander {
	case ExpanderFill:
		// The fullest group that is below its maximum.
		better = func(a, b *demandGroup) bool {
			return float64(a.projected)/float64(max(a.max, 1)) > float64(b.projected)/float64(max(b.max, 1))
		}
	case ExpanderPriority:
		better = func(a, b *demandGroup) bool { return a.priority < b.priority }
	case ExpanderWeighted:
		better = func(a, b *demandGroup) bool {
			return float64(a.weight)/float64(a.projected+1) > float64(b.weight)/float64(b.projected+1)
		}
	default: // spread
		better = func(a, b *demandGroup) bool { return a.projected < b.projected }
	}
	for _, g := range candidates[1:] {
		if better(g, best) {
			best = g
		}
	}
	return best
}

// scaleForDemand looks at the blocked work once.
func (r *runner) scaleForDemand(ctx context.Context) error {
	now := r.now()
	groups, err := r.readDemandGroups(now)
	if err != nil || len(groups) == 0 {
		return err
	}
	d := &demandRun{r: r, now: now, groups: groups, cfg: r.cfg.NodeScaling}
	var items []*demandItem
	if r.deps.BlockedDemand != nil {
		items = collectDemandItems(r.deps.BlockedDemand())
	}

	// The largest allocations first, so the packing of a bucket does not
	// depend on the order the work was found in.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].CPU+items[i].Memory > items[j].CPU+items[j].Memory
	})

	blocked := map[string]string{} // reasons to tell, by id
	units := 0
	for _, item := range items {
		if item.Unverifiable != "" {
			blocked["unverifiable/"+item.Namespace+"/"+item.Job+"/"+item.Group] = item.Unverifiable
			continue
		}
		var candidates []*demandGroup
		var rejected []string
		inPool := 0   // groups of the job's datacenters and pool (or spill)
		classOut := 0 // of those, groups of a class the job's constraint does not allow
		for _, g := range groups {
			if !slices.Contains(item.dcs, g.dc) {
				continue
			}
			// A job of a node pool uses the groups of that pool. A job
			// that may run in any pool ("all") uses the groups that allow
			// it with spill: by default work does not grow the groups of
			// pools it was not meant for.
			if g.pool != item.pool && !(item.pool == structs.NodePoolAll && g.spill) {
				continue
			}
			inPool++
			if !classAllowed(item.Classes, g.class) {
				classOut++
				continue
			}
			if !g.sizeKnown {
				rejected = append(rejected, g.name+": the size of a node is unknown (CapacityUnknown)")
				continue
			}
			restricted, ok := item.Unplaced.Restrict(&AdviceGroup{Pool: g.pool, Datacenter: g.dc, Class: g.class})
			if !ok {
				continue
			}
			if !restricted.fits(g.size) {
				rejected = append(rejected, g.name+": allocation too large for a node")
				blocked["toolarge/"+g.name+"/"+item.Namespace+"/"+item.Job+"/"+item.Group] = fmt.Sprintf(
					"an allocation of %.0f MHz, %.0f MB memory, %.0f MB disk does not fit an empty node of %s (%.0f MHz, %.0f MB, %.0f MB)",
					item.CPU, item.Memory, item.Disk, g.name, g.size.CPU, g.size.Memory, g.size.Disk)
				continue
			}
			candidates = append(candidates, g)
		}
		if len(candidates) == 0 {
			if item.pool == structs.NodePoolAll && inPool == 0 {
				blocked["nogroup/"+item.Namespace+"/"+item.Job+"/"+item.Group] =
					"the job may use any node pool; no node group allows it (spill = true) in its datacenters"
			}
			if len(rejected) == 0 && classOut > 0 && classOut == inPool {
				blocked["nogroup/"+item.Namespace+"/"+item.Job+"/"+item.Group] =
					"the node class of every node group of the job's pool and datacenters fails its constraint on ${node.class}"
			}
			if len(rejected) > 0 {
				blocked["nogroup/"+item.Namespace+"/"+item.Job+"/"+item.Group] = strings.Join(rejected, "; ")
			}
			continue
		}

		// How many allocations each group could take of this work: a
		// distinct_property caps it by the value the group's nodes would have
		// (one more in each datacenter that has room), and the work as a
		// whole by the allocations that are blocked.
		left := map[*demandGroup]int{}
		total := 0
		for _, g := range candidates {
			if rc, ok := item.Unplaced.Restrict(&AdviceGroup{Pool: g.pool, Datacenter: g.dc, Class: g.class}); ok {
				left[g] = rc.Count
				total += rc.Count
			}
		}
		count := min(item.Count, total)
		unit := item.Unplaced
		unit.Count = 1
		for i := 0; i < count && units < maxDemandUnits; i++ {
			units++
			// Work that fits a node that is already on its way (or one the
			// work opened) needs no new node, whatever the group is at.
			var room, open []*demandGroup
			for _, g := range candidates {
				if left[g] <= 0 {
					continue
				}
				if g.fitIndex(unit) >= 0 {
					room = append(room, g)
				}
				if g.projected < g.max {
					open = append(open, g)
				}
			}
			pick := room
			if len(pick) == 0 {
				pick = open
			}
			if len(pick) == 0 {
				blocked["atmax/"+item.pool+"/"+item.Job+"/"+item.Group] = "every group that could take the work is at its maximum"
				break
			}
			g := d.expand(item.pool, pick)
			g.place(unit)
			left[g]--
		}
	}

	d.apply(ctx, blocked)
	return nil
}

// apply raises the scale of the groups that got work, within the limits of
// the group and of its pool, and tells what could not be done.
func (d *demandRun) apply(ctx context.Context, blocked map[string]string) {
	r := d.r
	poolTotal := map[string]int64{}
	for _, g := range d.groups {
		poolTotal[g.pool] += g.spec
	}
	poolAdded := map[string]int64{}
	touched := map[string]bool{}

	for _, g := range d.groups {
		// The work that is blocked: the nodes it needs, less those on their
		// way. What is left of the nodes on their way is free capacity for
		// the headroom.
		need := int64(len(g.nodes))
		blockedAdd := g.opened()
		pendingLeft := g.pendingFree()

		// The headroom: free capacity worth a number of nodes ahead of the
		// work. It is an ahead-of-time trigger, so it waits for the cooldown
		// of the group; the blocked work does not.
		var headAdd int64
		var headReason string
		if g.headNodes > 0 || g.headPercent > 0 {
			wanted := max(float64(g.headNodes), g.headPercent/100*g.freeNodes)
			have := g.free + float64(pendingLeft)
			switch {
			case !g.freeKnown:
				// The gauges can't be read: no guess.
			case have < wanted:
				headAdd = int64(math.Ceil(wanted - have))
				headReason = fmt.Sprintf("headroom: %.1f nodes free (and %d on their way), %.1f wanted", g.free, pendingLeft, wanted)
			}
		}
		if headAdd > 0 && r.cooling(g) {
			headAdd = 0
		}

		total := blockedAdd + headAdd
		add := min(total, g.max-g.spec)
		ps := d.cfg.pool(g.pool)
		if ps.MaxStep > 0 {
			add = min(add, int64(ps.MaxStep)-poolAdded[g.pool])
		}
		if ps.MaxNodes > 0 {
			add = min(add, int64(ps.MaxNodes)-poolTotal[g.pool]-poolAdded[g.pool])
		}
		if add < total {
			blocked["limit/"+g.name] = fmt.Sprintf("%s needs %d more nodes and may add %d (maximum of the group %d, of the pool %d, step %d)",
				g.name, total, max(add, 0), g.max, ps.MaxNodes, ps.MaxStep)
		}
		if add <= 0 {
			continue
		}
		poolAdded[g.pool] += add
		touched[g.name] = true

		desired := g.spec + add
		if r.lastDemand[g.name] == desired && g.policy.Target["dry-run"] == "true" {
			continue
		}
		var reasons []string
		if blockedAdd > 0 {
			reasons = append(reasons, fmt.Sprintf("blocked work needs %d nodes (%d already asked for): %s", need, g.pending, demandJobs(g.bucket)))
		}
		if headAdd > 0 {
			reasons = append(reasons, headReason)
		}
		check := checkBlockedDemand
		if blockedAdd == 0 {
			check = checkHeadroom
		}
		if err := r.scaleGroupUp(g, desired, strings.Join(reasons, "; ")+fmt.Sprintf(" [%d ready, spec %d]", g.ready, g.spec), check); err != nil {
			r.logger.Warn("failed to raise the scale of a node group", "group", g.name, "error", err)
			continue
		}
		r.lastDemand[g.name] = desired
	}
	for name := range r.lastDemand {
		if !touched[name] {
			delete(r.lastDemand, name)
		}
	}

	// Tell what is not done, once per reason.
	seen := map[string]bool{}
	for id, reason := range blocked {
		seen[id] = true
		if r.told[id] == reason {
			continue
		}
		r.told[id] = reason
		typ, ev := d.event(id, reason)
		if r.deps.PublishEvent != nil {
			ev.Time = d.now.UnixNano()
			r.deps.PublishEvent(typ, ev)
		}
	}
	for id := range r.told {
		if !seen[id] {
			delete(r.told, id)
		}
	}
}

// event builds the event that tells a reason why work is not helped.
func (d *demandRun) event(id, reason string) (string, *structs.ScalingDecisionEvent) {
	kind, rest, _ := strings.Cut(id, "/")
	ev := &structs.ScalingDecisionEvent{Direction: "up", Reason: reason}
	typ := structs.TypeScalingNodeBlocked
	switch kind {
	case "unverifiable":
		typ = structs.TypeScalingNodeUnverifiable
		ev.Reason = "the constraint " + reason + " cannot be checked against a node that does not exist yet, so no node is added for it"
		fillWork(ev, rest)
	case "toolarge":
		typ = structs.TypeScalingNodeAllocationTooLarge
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 {
			fillWork(ev, parts[1])
		}
	case "nogroup":
		fillWork(ev, rest)
	case "atmax":
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 {
			ev.Group = parts[0]
		}
	case "limit":
		ev.Group = rest
	}
	return typ, ev
}

// fillWork sets the job of an event from "namespace/job/group".
func fillWork(ev *structs.ScalingDecisionEvent, id string) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) == 3 {
		ev.Namespace, ev.JobID, ev.Group = parts[0], parts[1], parts[2]
	}
}

// demandJobs lists the jobs of the work of a bucket for the reason of a
// decision.
func demandJobs(bucket []Unplaced) string {
	seen := map[string]bool{}
	var jobs []string
	for _, u := range bucket {
		id := u.Namespace + "/" + u.Job
		if !seen[id] {
			seen[id] = true
			jobs = append(jobs, id)
		}
	}
	slices.Sort(jobs)
	if len(jobs) > 5 {
		jobs = append(jobs[:5], fmt.Sprintf("and %d more", len(jobs)-5))
	}
	return strings.Join(jobs, ", ")
}

// scaleGroupUp hands the decision to the executor, which applies the rate
// limit, the oscillation guard and the dry run of the group.
func (r *runner) scaleGroupUp(g *demandGroup, desired int64, reason, check string) error {
	c, t, err := r.groupTarget(g)
	if err != nil {
		return err
	}
	action := sdk.ScalingAction{
		Count:     desired,
		Direction: sdk.ScaleDirectionUp,
		Reason:    reason,
	}
	return r.exec.execute(c, t, action, check, r.now())
}

// groupTarget returns what the executor needs to scale a node group: its
// policy and the state of the object.
func (r *runner) groupTarget(g *demandGroup) (*compiledPolicy, *targetState, error) {
	key, _ := targetKeyFromPolicy(g.policy)
	object, err := objectTargetFromPolicy(g.policy)
	if err != nil || object == nil {
		return nil, nil, fmt.Errorf("no object target: %v", err)
	}
	t, err := readObjectTarget(r.deps.State(), key, object)
	if err != nil || t == nil {
		return nil, nil, fmt.Errorf("reading the group: %v", err)
	}

	sp := parsePolicy(g.policy)
	sp.Target = parseTarget(nil, g.policy.Target)
	sp.Target.Name = "nomad"
	r.proc.canonicalizePolicy(&sp)
	return &compiledPolicy{policy: &sp, key: key, object: object}, t, nil
}

// cooling reports whether the group is in its cooldown or suspended, which
// holds the ahead-of-time triggers.
func (r *runner) cooling(g *demandGroup) bool {
	c, t, err := r.groupTarget(g)
	if err != nil {
		return true
	}
	until, _ := r.exec.blockedUntil(c.key, c.policy, t)
	return r.now().Before(until)
}

// demandLoop looks at the blocked work every interval until ctx is canceled.
func (r *runner) demandLoop(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.NodeScaling.interval())
	defer ticker.Stop()
	for {
		if err := r.scaleForDemand(ctx); err != nil {
			r.logger.Warn("looking at the blocked work failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
