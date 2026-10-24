// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"fmt"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// This file implements the colocate and separate (cross-job affinity and
// anti-affinity) rules of task groups:
//
//   - allocAffinityTracker collects, once per evaluation and task group, the
//     non-terminal allocations selected by each rule and counts them per
//     topology domain. Before every placement it folds the current plan
//     into the counts incrementally, so placements of the same evaluation
//     count too.
//   - AllocAffinityFeasibilityIterator applies the required rules as a
//     feasibility filter.
//   - AllocAffinityRankIterator applies the preferred rules as a score.
//
// The cost of a placement check is a map lookup per rule and node; the
// collection cost is proportional to the number of jobs in the target
// namespace and to the number of matching allocations, not to the number of
// nodes.

const (
	// allocAffinityScorerName is the name of the score in the AllocMetric.
	allocAffinityScorerName = "alloc-affinity"

	// allocAffinityManyJobs is the number of jobs selected by a single rule
	// above which the scheduler logs a warning, because the collection cost
	// grows with the number of matching allocations.
	allocAffinityManyJobs = 1000
)

// allocAffinityTracker holds the per task group domain counts of the colocate
// and separate rules. It is shared by the feasibility and rank iterators of a
// stack; the stack drives it through SetJob and SetTaskGroup.
type allocAffinityTracker struct {
	ctx Context
	job *structs.Job

	// groups is the memoized state by task group name. It is reset when the
	// job changes.
	groups map[string]*allocAffinityGroup

	// current is the state of the task group being placed, nil if the group
	// has no rules.
	current *allocAffinityGroup

	// nodes caches node lookups for the allocations of the state and plan.
	nodes map[string]*structs.Node
}

// allocAffinityGroup is the state of the rules of a single task group.
type allocAffinityGroup struct {
	name  string
	rules []*allocAffinityRule

	hasRequired  bool
	hasPreferred bool

	// sumWeight is the sum of the absolute weights of the preferred rules.
	sumWeight float64

	// planDependent is true if any rule selects a task group of the job
	// being evaluated, in which case placements and stops of the plan
	// change the counts. Otherwise only preemptions do.
	planDependent bool

	// planNodes is the contribution of each node of the plan to the counts,
	// used to fold in only the nodes of the plan that changed.
	planNodes map[string]*allocAffinityPlanNode
	planGen   uint64

	// err is set if the existing allocations could not be collected.
	err error
}

// allocAffinityRule is a colocate or separate rule with its domain counts.
type allocAffinityRule struct {
	rule     *structs.AllocAffinity
	colocate bool

	// name identifies the rule in placement metrics.
	name string

	// selfMatch is true if the task group being placed is selected by the
	// rule, which lets the first allocation of a required colocate rule be
	// placed when nothing matches yet.
	selfMatch bool

	// ownGroups is the set of task groups of the evaluated job selected by
	// the rule.
	ownGroups map[string]struct{}

	// existing is the number of matching allocations in the state by
	// domain, and existingIDs the set of their IDs.
	existing    map[string]int
	existingIDs map[string]struct{}

	// proposed is the change of the counts by the plan by domain.
	proposed map[string]int

	// total is the number of matching allocations, including the plan.
	total int
}

// allocAffinityPlanNode is the contribution of one node of the plan.
type allocAffinityPlanNode struct {
	gen     uint64
	fp      planNodeFingerprint
	domains []string
	deltas  []int
}

// planNodeFingerprint identifies the plan entries of a node. The schedulers
// only append to, or pop the last element from, these slices, so the lengths
// and last elements identify their contents.
type planNodeFingerprint struct {
	nAlloc, nUpdate, nPreempt          int
	lastAlloc, lastUpdate, lastPreempt *structs.Allocation
}

func newAllocAffinityTracker(ctx Context) *allocAffinityTracker {
	return &allocAffinityTracker{
		ctx:    ctx,
		groups: make(map[string]*allocAffinityGroup),
		nodes:  make(map[string]*structs.Node),
	}
}

// SetJob sets the job being evaluated and drops the memoized state.
func (t *allocAffinityTracker) SetJob(job *structs.Job) {
	t.job = job
	t.groups = make(map[string]*allocAffinityGroup)
	t.current = nil
}

// SetTaskGroup sets the task group to place. The existing allocations are
// collected the first time a group is seen; the plan is folded in on every
// call, so it must be called before every placement.
func (t *allocAffinityTracker) SetTaskGroup(tg *structs.TaskGroup) {
	t.current = nil
	if tg == nil || !tg.HasAllocAffinities() || t.job == nil {
		return
	}

	g, ok := t.groups[tg.Name]
	if !ok {
		g = t.buildGroup(tg)
		t.groups[tg.Name] = g
	}
	t.current = g
	if g.err == nil {
		t.refreshPlan(g)
	}
}

func (t *allocAffinityTracker) hasRequired() bool {
	return t.current != nil && t.current.hasRequired
}

func (t *allocAffinityTracker) hasPreferred() bool {
	return t.current != nil && t.current.hasPreferred
}

// buildGroup collects the existing allocations selected by the rules of the
// task group.
func (t *allocAffinityTracker) buildGroup(tg *structs.TaskGroup) *allocAffinityGroup {
	g := &allocAffinityGroup{
		name:      tg.Name,
		planNodes: make(map[string]*allocAffinityPlanNode),
	}

	add := func(a *structs.AllocAffinity, colocate bool) {
		kind := structs.AllocAffinityKindSeparate
		if colocate {
			kind = structs.AllocAffinityKindColocate
		}
		r := &allocAffinityRule{
			rule:        a,
			colocate:    colocate,
			name:        kind + " " + a.String(),
			ownGroups:   make(map[string]struct{}),
			existing:    make(map[string]int),
			existingIDs: make(map[string]struct{}),
			proposed:    make(map[string]int),
		}
		if a.TargetNamespace(t.job.Namespace) == t.job.Namespace {
			for _, own := range t.job.TaskGroups {
				if a.Matches(t.job.Meta, own.Meta) {
					r.ownGroups[own.Name] = struct{}{}
				}
			}
			_, r.selfMatch = r.ownGroups[tg.Name]
		}
		if len(r.ownGroups) > 0 {
			g.planDependent = true
		}
		if a.Required {
			g.hasRequired = true
		} else if a.Weight != 0 {
			g.hasPreferred = true
			if a.Weight < 0 {
				g.sumWeight -= float64(a.Weight)
			} else {
				g.sumWeight += float64(a.Weight)
			}
		}
		g.rules = append(g.rules, r)
	}
	for _, a := range tg.Colocate {
		add(a, true)
	}
	for _, a := range tg.Separate {
		add(a, false)
	}

	// Collect per target namespace, so that rules sharing a namespace share
	// the scan of its jobs and the lookup of their allocations.
	byNamespace := make(map[string][]*allocAffinityRule)
	var namespaces []string
	for _, r := range g.rules {
		ns := r.rule.TargetNamespace(t.job.Namespace)
		if _, ok := byNamespace[ns]; !ok {
			namespaces = append(namespaces, ns)
		}
		byNamespace[ns] = append(byNamespace[ns], r)
	}
	for _, ns := range namespaces {
		if err := t.collect(ns, byNamespace[ns]); err != nil {
			g.err = err
			t.ctx.Logger().Named("alloc_affinity").Error(
				"failed to collect allocations for colocate/separate rules",
				"task_group", tg.Name, "namespace", ns, "error", err)
			break
		}
	}
	return g
}

// collect counts the non-terminal allocations of the namespace selected by
// the rules by domain.
func (t *allocAffinityTracker) collect(namespace string, rules []*allocAffinityRule) error {
	ws := memdb.NewWatchSet()
	iter, err := t.ctx.State().JobsByNamespace(ws, namespace)
	if err != nil {
		return fmt.Errorf("failed to list jobs: %v", err)
	}

	matchedJobs := make([]int, len(rules))
	matched := make([]map[string]struct{}, len(rules))
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		job := raw.(*structs.Job)
		if job.Namespace == t.job.Namespace && job.ID == t.job.ID {
			// Use the version being evaluated for the job's own groups.
			job = t.job
		}

		selected := false
		for i, r := range rules {
			matched[i] = nil
			for _, tg := range job.TaskGroups {
				if r.rule.Matches(job.Meta, tg.Meta) {
					if matched[i] == nil {
						matched[i] = make(map[string]struct{})
					}
					matched[i][tg.Name] = struct{}{}
				}
			}
			if matched[i] != nil {
				matchedJobs[i]++
				selected = true
			}
		}
		if !selected {
			continue
		}

		allocs, err := t.ctx.State().AllocsByJob(ws, namespace, job.ID, false)
		if err != nil {
			return fmt.Errorf("failed to get allocations of job %q: %v", job.ID, err)
		}
		for _, alloc := range allocs {
			if alloc.TerminalStatus() {
				continue
			}
			var node *structs.Node
			for i, r := range rules {
				if _, ok := matched[i][alloc.TaskGroup]; !ok {
					continue
				}
				if node == nil {
					if node, err = t.node(alloc.NodeID); err != nil {
						return err
					}
				}
				domain, ok := getProperty(node, r.rule.Scope)
				if !ok {
					continue
				}
				r.existing[domain]++
				r.existingIDs[alloc.ID] = struct{}{}
				r.total++
			}
		}
	}

	for i, r := range rules {
		if matchedJobs[i] > allocAffinityManyJobs {
			t.ctx.Logger().Named("alloc_affinity").Warn(
				"colocate/separate rule selects a large number of jobs",
				"rule", r.name, "jobs", matchedJobs[i])
		}
	}
	return nil
}

// node returns the node by ID, from the cache if possible. A node that does
// not exist (anymore) is returned as nil.
func (t *allocAffinityTracker) node(id string) (*structs.Node, error) {
	if n, ok := t.nodes[id]; ok {
		return n, nil
	}
	n, err := t.ctx.State().NodeByID(nil, id)
	if err != nil {
		return nil, fmt.Errorf("failed to look up node %q: %v", id, err)
	}
	t.nodes[id] = n
	return n, nil
}

// refreshPlan folds the nodes of the plan that changed since the previous
// call into the proposed counts of the group.
func (t *allocAffinityTracker) refreshPlan(g *allocAffinityGroup) {
	plan := t.ctx.Plan()
	if plan == nil {
		return
	}
	g.planGen++

	visit := func(nodeID string) {
		fp := planNodeFingerprint{}
		if l := plan.NodeAllocation[nodeID]; len(l) > 0 && g.planDependent {
			fp.nAlloc, fp.lastAlloc = len(l), l[len(l)-1]
		}
		if l := plan.NodeUpdate[nodeID]; len(l) > 0 && g.planDependent {
			fp.nUpdate, fp.lastUpdate = len(l), l[len(l)-1]
		}
		if l := plan.NodePreemptions[nodeID]; len(l) > 0 {
			fp.nPreempt, fp.lastPreempt = len(l), l[len(l)-1]
		}

		pn, ok := g.planNodes[nodeID]
		if ok {
			if pn.gen == g.planGen {
				return
			}
			pn.gen = g.planGen
			if pn.fp == fp {
				return
			}
			g.applyPlanNode(pn, -1)
		} else {
			pn = &allocAffinityPlanNode{gen: g.planGen}
			g.planNodes[nodeID] = pn
		}
		pn.fp = fp
		t.computePlanNode(g, pn, nodeID, plan)
		g.applyPlanNode(pn, 1)
	}

	if g.planDependent {
		for nodeID := range plan.NodeAllocation {
			visit(nodeID)
		}
		for nodeID := range plan.NodeUpdate {
			visit(nodeID)
		}
	}
	for nodeID := range plan.NodePreemptions {
		visit(nodeID)
	}

	// Remove the nodes that left the plan.
	if len(g.planNodes) > 0 {
		for nodeID, pn := range g.planNodes {
			if pn.gen != g.planGen {
				g.applyPlanNode(pn, -1)
				delete(g.planNodes, nodeID)
			}
		}
	}
}

// computePlanNode computes the contribution of a node of the plan to the
// count of each rule: matching placements count, stopped and preempted
// matching allocations of the state are discounted.
func (t *allocAffinityTracker) computePlanNode(g *allocAffinityGroup, pn *allocAffinityPlanNode, nodeID string, plan *structs.Plan) {
	if pn.domains == nil {
		pn.domains = make([]string, len(g.rules))
		pn.deltas = make([]int, len(g.rules))
	}
	node, err := t.node(nodeID)
	if err != nil {
		t.ctx.Logger().Named("alloc_affinity").Error("failed to look up plan node", "error", err)
	}
	for i, r := range g.rules {
		pn.deltas[i] = 0
		domain, ok := getProperty(node, r.rule.Scope)
		if !ok {
			continue
		}
		pn.domains[i] = domain

		delta := 0
		if g.planDependent {
			for _, alloc := range plan.NodeAllocation[nodeID] {
				if _, existing := r.existingIDs[alloc.ID]; existing {
					// in-place update of a counted allocation
					continue
				}
				if _, ok := r.ownGroups[alloc.TaskGroup]; ok {
					delta++
				}
			}
			for _, alloc := range plan.NodeUpdate[nodeID] {
				if _, existing := r.existingIDs[alloc.ID]; existing {
					delta--
				}
			}
		}
		for _, alloc := range plan.NodePreemptions[nodeID] {
			if _, existing := r.existingIDs[alloc.ID]; existing {
				delta--
			}
		}
		pn.deltas[i] = delta
	}
}

// applyPlanNode adds (sign 1) or removes (sign -1) the contribution of a node
// of the plan.
func (g *allocAffinityGroup) applyPlanNode(pn *allocAffinityPlanNode, sign int) {
	for i, delta := range pn.deltas {
		if delta == 0 {
			continue
		}
		r := g.rules[i]
		r.proposed[pn.domains[i]] += sign * delta
		r.total += sign * delta
	}
}

// count returns the number of matching allocations, including the plan, in
// the domain.
func (r *allocAffinityRule) count(domain string) int {
	if c := r.existing[domain] + r.proposed[domain]; c > 0 {
		return c
	}
	return 0
}

// isInplaceProbe returns whether the current selection checks whether an
// allocation of the task group can be updated in place on the node. The
// in-place update logic stages the stop of the allocation on the node for the
// duration of the selection. The rules apply only to new placements, so
// they never force an in-place update to become destructive.
func isInplaceProbe(plan *structs.Plan, node *structs.Node, tgName string) bool {
	if plan == nil {
		return false
	}
	updates := plan.NodeUpdate[node.ID]
	if n := len(updates); n > 0 {
		last := updates[n-1]
		return last.DesiredDescription == allocInPlace && last.TaskGroup == tgName
	}
	return false
}

// satisfiesRequired returns whether the node satisfies the required rules of
// the current task group, and if not the name of the first failing rule.
func (t *allocAffinityTracker) satisfiesRequired(node *structs.Node) (bool, string) {
	g := t.current
	if g.err != nil {
		return false, "colocate/separate: " + g.err.Error()
	}
	for _, r := range g.rules {
		if !r.rule.Required {
			continue
		}
		domain, ok := getProperty(node, r.rule.Scope)
		if !ok {
			return false, r.name + ": missing property"
		}
		present := r.count(domain) > 0
		if r.colocate {
			// Like Kubernetes, the first allocation of a group selected by
			// its own colocate rule may be placed anywhere.
			if !present && !(r.selfMatch && r.total <= 0) {
				return false, r.name
			}
		} else if present {
			return false, r.name
		}
	}
	return true, ""
}

// score returns the normalized preference score of the node, in [-1, 1].
func (t *allocAffinityTracker) score(node *structs.Node) float64 {
	g := t.current
	if g.err != nil || g.sumWeight == 0 {
		return 0
	}
	total := 0.0
	for _, r := range g.rules {
		if r.rule.Required {
			continue
		}
		domain, ok := getProperty(node, r.rule.Scope)
		if !ok || r.count(domain) == 0 {
			continue
		}
		if r.colocate {
			total += float64(r.rule.Weight)
		} else {
			total -= float64(r.rule.Weight)
		}
	}
	return total / g.sumWeight
}

// AllocAffinityFeasibilityIterator is a FeasibleIterator that filters out the
// nodes violating the required colocate and separate rules of the task
// group.
type AllocAffinityFeasibilityIterator struct {
	ctx     Context
	source  FeasibleIterator
	tracker *allocAffinityTracker
	tgName  string
}

// NewAllocAffinityFeasibilityIterator creates an iterator applying the
// required rules tracked by the tracker.
func NewAllocAffinityFeasibilityIterator(ctx Context, source FeasibleIterator, tracker *allocAffinityTracker) *AllocAffinityFeasibilityIterator {
	return &AllocAffinityFeasibilityIterator{
		ctx:     ctx,
		source:  source,
		tracker: tracker,
	}
}

// SetTaskGroup sets the task group being placed. The stack updates the
// shared tracker separately.
func (iter *AllocAffinityFeasibilityIterator) SetTaskGroup(tg *structs.TaskGroup) {
	iter.tgName = tg.Name
}

func (iter *AllocAffinityFeasibilityIterator) Next() *structs.Node {
	for {
		option := iter.source.Next()

		// Hot path if there is nothing to check
		if option == nil || !iter.tracker.hasRequired() {
			return option
		}
		if isInplaceProbe(iter.ctx.Plan(), option, iter.tgName) {
			return option
		}
		if ok, reason := iter.tracker.satisfiesRequired(option); !ok {
			iter.ctx.Metrics().FilterNode(option, reason)
			continue
		}
		return option
	}
}

func (iter *AllocAffinityFeasibilityIterator) Reset() {
	iter.source.Reset()
}

// AllocAffinityRankIterator is a RankIterator that scores nodes by the
// preferred colocate and separate rules of the task group. The score is the
// sum of the weights of the colocate rules with a matching allocation in the
// node's domain, minus those of the separate rules, normalized by the sum of
// the absolute weights like the node affinity score.
type AllocAffinityRankIterator struct {
	ctx     Context
	source  RankIterator
	tracker *allocAffinityTracker
}

// NewAllocAffinityRankIterator creates an iterator applying the preferred
// rules tracked by the tracker.
func NewAllocAffinityRankIterator(ctx Context, source RankIterator, tracker *allocAffinityTracker) *AllocAffinityRankIterator {
	return &AllocAffinityRankIterator{
		ctx:     ctx,
		source:  source,
		tracker: tracker,
	}
}

func (iter *AllocAffinityRankIterator) Next() *RankedNode {
	option := iter.source.Next()
	if option == nil || !iter.tracker.hasPreferred() {
		return option
	}

	// The score is appended for every node, including those without a
	// match, so that the preference shifts the normalized score relative to
	// the other nodes in proportion to its weight.
	score := iter.tracker.score(option.Node)
	option.Scores = append(option.Scores, score)
	iter.ctx.Metrics().ScoreNode(option.Node, allocAffinityScorerName, score)
	return option
}

func (iter *AllocAffinityRankIterator) Reset() {
	iter.source.Reset()
}
