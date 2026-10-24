// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// affinityJob returns a minimal service job whose job meta carries app=<app>.
func affinityJob(app string, count int) *structs.Job {
	job := mock.Job()
	job.Meta = map[string]string{"app": app}
	tg := job.TaskGroups[0]
	tg.Count = count
	tg.Networks = nil
	tg.Update = nil
	for _, task := range tg.Tasks {
		task.Services = nil
	}
	return job
}

// affinityNodes upserts zones*perZone nodes with meta.zone set to z0, z1, ...
func affinityNodes(t testing.TB, h *Harness, zones, perZone int) []*structs.Node {
	var nodes []*structs.Node
	for z := 0; z < zones; z++ {
		for i := 0; i < perZone; i++ {
			node := mock.Node()
			node.Meta["zone"] = fmt.Sprintf("z%d", z)
			must.NoError(t, node.ComputeClass())
			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// affinityAlloc returns a running allocation of the job's first task group on
// the node, using cpu CPU shares.
func affinityAlloc(job *structs.Job, node *structs.Node, index, cpu int) *structs.Allocation {
	tg := job.TaskGroups[0]
	return &structs.Allocation{
		ID:        uuid.Generate(),
		EvalID:    uuid.Generate(),
		Name:      structs.AllocName(job.ID, tg.Name, uint(index)),
		Namespace: job.Namespace,
		JobID:     job.ID,
		Job:       job,
		TaskGroup: tg.Name,
		NodeID:    node.ID,
		AllocatedResources: &structs.AllocatedResources{
			Tasks: map[string]*structs.AllocatedTaskResources{
				tg.Tasks[0].Name: {
					Cpu:    structs.AllocatedCpuResources{CpuShares: int64(cpu)},
					Memory: structs.AllocatedMemoryResources{MemoryMB: 256},
				},
			},
			Shared: structs.AllocatedSharedResources{DiskMB: 150},
		},
		DesiredStatus: structs.AllocDesiredStatusRun,
		ClientStatus:  structs.AllocClientStatusRunning,
	}
}

// upsertRunning upserts the job and a running allocation of it on each node.
func upsertRunning(t testing.TB, h *Harness, job *structs.Job, cpu int, nodes ...*structs.Node) []*structs.Allocation {
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	var allocs []*structs.Allocation
	for i, node := range nodes {
		allocs = append(allocs, affinityAlloc(job, node, i, cpu))
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))
	return allocs
}

// registerAndProcess upserts the job and processes a registration
// evaluation for it with the matching scheduler.
func registerAndProcess(t testing.TB, h *Harness, job *structs.Job) {
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	eval := &structs.Evaluation{
		Namespace:   job.Namespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		Type:        job.Type,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	factory := NewServiceScheduler
	switch job.Type {
	case structs.JobTypeSystem:
		factory = NewSystemScheduler
	case structs.JobTypeBatch:
		factory = NewBatchScheduler
	}
	must.NoError(t, h.Process(factory, eval))
}

// placedByNode returns the number of allocations of the last plan by node.
func placedByNode(t testing.TB, h *Harness) map[string]int {
	must.SliceNotEmpty(t, h.Plans)
	out := make(map[string]int)
	for nodeID, allocs := range h.Plans[len(h.Plans)-1].NodeAllocation {
		out[nodeID] += len(allocs)
	}
	return out
}

func zoneOf(nodes []*structs.Node, id string) string {
	for _, n := range nodes {
		if n.ID == id {
			return n.Meta["zone"]
		}
	}
	return ""
}

func TestAllocAffinity_SeparateRequired_OwnReplicas(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 3, 2)

	rule := &structs.AllocAffinity{
		Selector: map[string]string{"app": "cache"},
		Scope:    "${meta.zone}",
		Required: true,
	}
	job := affinityJob("cache", 3)
	job.TaskGroups[0].Separate = []*structs.AllocAffinity{rule}
	registerAndProcess(t, h, job)

	placed := placedByNode(t, h)
	zones := make(map[string]int)
	for nodeID, n := range placed {
		zones[zoneOf(nodes, nodeID)] += n
	}
	must.Eq(t, map[string]int{"z0": 1, "z1": 1, "z2": 1}, zones)
	must.MapEmpty(t, h.Evals[0].FailedTGAllocs)

	// A fourth replica has no free zone left: the placement fails with the
	// rule as the reason and a blocked evaluation is created.
	job2 := job.Copy()
	job2.TaskGroups[0].Count = 4
	job2.Version++
	h.Plans, h.Evals, h.CreateEvals = nil, nil, nil
	registerAndProcess(t, h, job2)

	must.Len(t, 1, h.Evals)
	failed := h.Evals[0].FailedTGAllocs["web"]
	must.NotNil(t, failed)
	must.Eq(t, 6, failed.ConstraintFiltered["separate {app=cache} by ${meta.zone}"])
	must.Len(t, 1, h.CreateEvals)
	must.Eq(t, structs.EvalStatusBlocked, h.CreateEvals[0].Status)
}

func TestAllocAffinity_ColocateRequired_OtherJob(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 5)
	api := affinityJob("api", 1)
	upsertRunning(t, h, api, 500, nodes[2])

	job := affinityJob("cache", 2)
	job.TaskGroups[0].Colocate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Required: true,
	}}
	registerAndProcess(t, h, job)
	must.Eq(t, map[string]int{nodes[2].ID: 2}, placedByNode(t, h))

	// Without any matching allocation, nothing is placed: the job does not
	// select itself, so the bootstrap exception does not apply.
	h2 := NewHarness(t)
	affinityNodes(t, h2, 1, 5)
	registerAndProcess(t, h2, job.Copy())
	failed := h2.Evals[0].FailedTGAllocs["web"]
	must.NotNil(t, failed)
	must.Eq(t, 5, failed.ConstraintFiltered["colocate {app=api} by ${node.unique.id}"])
}

func TestAllocAffinity_AllocOnMissingNode(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 2)

	// An allocation whose node is gone from the state does not count.
	api := affinityJob("api", 1)
	gone := mock.Node()
	upsertRunning(t, h, api, 500, gone)

	job := affinityJob("cache", 1)
	job.TaskGroups[0].Separate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Required: true,
	}}
	registerAndProcess(t, h, job)
	placed := placedByNode(t, h)
	must.MapLen(t, 1, placed)
	must.MapNotContainsKey(t, placed, gone.ID)
	must.MapContainsKey(t, map[string]bool{nodes[0].ID: true, nodes[1].ID: true}, func() string {
		for id := range placed {
			return id
		}
		return ""
	}())
}

func TestAllocAffinity_ColocateRequired_SelfBootstrap(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 3, 3)

	job := affinityJob("cache", 4)
	job.TaskGroups[0].Colocate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "cache"},
		Scope:    "${meta.zone}",
		Required: true,
	}}
	registerAndProcess(t, h, job)

	zones := make(map[string]int)
	for nodeID, n := range placedByNode(t, h) {
		zones[zoneOf(nodes, nodeID)] += n
	}
	must.MapLen(t, 1, zones)
	for _, n := range zones {
		must.Eq(t, 4, n)
	}
}

func TestAllocAffinity_SeparatePreferred(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 2)
	api := affinityJob("api", 1)
	upsertRunning(t, h, api, 1000, nodes[0])

	// Without the rule, bin packing prefers the node already in use.
	job := affinityJob("cache", 1)
	registerAndProcess(t, h, job)
	must.Eq(t, map[string]int{nodes[0].ID: 1}, placedByNode(t, h))

	// With the rule, the other node wins.
	h2 := NewHarness(t)
	for _, n := range nodes {
		must.NoError(t, h2.State.UpsertNode(structs.MsgTypeTestSetup, h2.NextIndex(), n.Copy()))
	}
	upsertRunning(t, h2, api, 1000, nodes[0])
	job2 := affinityJob("cache", 1)
	job2.TaskGroups[0].Separate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Weight:   100,
	}}
	registerAndProcess(t, h2, job2)
	must.Eq(t, map[string]int{nodes[1].ID: 1}, placedByNode(t, h2))
}

func TestAllocAffinity_ColocatePreferred(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 10)
	api := affinityJob("api", 1)
	upsertRunning(t, h, api, 500, nodes[7])

	job := affinityJob("cache", 1)
	job.TaskGroups[0].Colocate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Weight:   50,
	}}
	registerAndProcess(t, h, job)
	must.Eq(t, map[string]int{nodes[7].ID: 1}, placedByNode(t, h))

	// The score is recorded in the placement metrics.
	alloc := h.Plans[0].NodeAllocation[nodes[7].ID][0]
	var found bool
	for _, meta := range alloc.Metrics.ScoreMetaData {
		if meta.NodeID == nodes[7].ID {
			must.Eq(t, 1.0, meta.Scores[allocAffinityScorerName])
			found = true
		}
	}
	must.True(t, found)
}

func TestAllocAffinity_PreferenceDoesNotOverrideResources(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 3)

	// The api allocation uses all the CPU of its node.
	api := affinityJob("api", 1)
	upsertRunning(t, h, api, 3900, nodes[1])

	job := affinityJob("cache", 1)
	job.TaskGroups[0].Colocate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Weight:   100,
	}}
	registerAndProcess(t, h, job)
	placed := placedByNode(t, h)
	must.MapLen(t, 1, placed)
	must.MapNotContainsKey(t, placed, nodes[1].ID)
	must.MapEmpty(t, h.Evals[0].FailedTGAllocs)
}

func TestAllocAffinity_OtherNamespace(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 4)

	ns := mock.Namespace()
	ns.Name = "other"
	must.NoError(t, h.State.UpsertNamespaces(h.NextIndex(), []*structs.Namespace{ns}))
	api := affinityJob("api", 1)
	api.Namespace = "other"
	upsertRunning(t, h, api, 500, nodes[3])

	// A rule without namespace only looks into the job's own namespace.
	job := affinityJob("cache", 1)
	job.TaskGroups[0].Colocate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Required: true,
	}}
	registerAndProcess(t, h, job)
	must.NotNil(t, h.Evals[0].FailedTGAllocs["web"])

	h.Plans, h.Evals, h.CreateEvals = nil, nil, nil
	job2 := job.Copy()
	job2.Version++
	job2.TaskGroups[0].Colocate[0].Namespace = "other"
	registerAndProcess(t, h, job2)
	must.Eq(t, map[string]int{nodes[3].ID: 1}, placedByNode(t, h))
}

func TestAllocAffinity_SystemJob_SeparateRequired(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 4)
	api := affinityJob("api", 1)
	upsertRunning(t, h, api, 500, nodes[0])

	job := mock.SystemJob()
	job.TaskGroups[0].Separate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Required: true,
	}}
	registerAndProcess(t, h, job)

	placed := placedByNode(t, h)
	must.MapLen(t, 3, placed)
	must.MapNotContainsKey(t, placed, nodes[0].ID)
}

func TestAllocAffinity_DestructiveUpdate_OwnReplicas(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 3, 1)

	job := affinityJob("cache", 3)
	job.TaskGroups[0].Separate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "cache"},
		Scope:    "${meta.zone}",
		Required: true,
	}}
	upsertRunning(t, h, job, 500, nodes...)

	// A destructive update replaces every replica: the stopped replicas of
	// the plan no longer count, so each zone can take a new one.
	job2 := job.Copy()
	job2.Version++
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	registerAndProcess(t, h, job2)

	plan := h.Plans[0]
	stopped := 0
	for _, allocs := range plan.NodeUpdate {
		stopped += len(allocs)
	}
	must.Eq(t, 3, stopped)
	must.Eq(t, map[string]int{nodes[0].ID: 1, nodes[1].ID: 1, nodes[2].ID: 1}, placedByNode(t, h))
	must.MapEmpty(t, h.Evals[0].FailedTGAllocs)
}

func TestAllocAffinity_InplaceUpdate_NotMoved(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	nodes := affinityNodes(t, h, 1, 2)

	job := affinityJob("cache", 1)
	job.TaskGroups[0].Separate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    "${node.unique.id}",
		Required: true,
	}}
	existing := upsertRunning(t, h, job, 500, nodes[0])

	// An api allocation arrives next to the cache after it was placed.
	api := affinityJob("api", 1)
	upsertRunning(t, h, api, 500, nodes[0])

	// An in-place update keeps the allocation where it is: the rules only
	// apply to new placements.
	job2 := job.Copy()
	job2.Version++
	job2.TaskGroups[0].Tasks[0].KillTimeout = 42 * time.Second
	registerAndProcess(t, h, job2)

	plan := h.Plans[0]
	must.MapEmpty(t, plan.NodeUpdate)
	must.Len(t, 1, plan.NodeAllocation[nodes[0].ID])
	must.Eq(t, existing[0].ID, plan.NodeAllocation[nodes[0].ID][0].ID)
}

func TestAllocAffinity_NoRulesHotPath(t *testing.T) {
	ci.Parallel(t)

	_, ctx := testContext(t)
	tracker := newAllocAffinityTracker(ctx)
	job := affinityJob("cache", 1)
	tracker.SetJob(job)
	tracker.SetTaskGroup(job.TaskGroups[0])
	must.False(t, tracker.hasRequired())
	must.False(t, tracker.hasPreferred())
	must.MapEmpty(t, tracker.groups)
}

// TestAllocAffinity_PlanIncremental checks that the tracker folds plan
// changes in, including pops of staged stops.
func TestAllocAffinity_PlanIncremental(t *testing.T) {
	ci.Parallel(t)

	state, ctx := testContext(t)
	nodes := []*structs.Node{mock.Node(), mock.Node()}
	for i, n := range nodes {
		n.Meta["zone"] = fmt.Sprintf("z%d", i)
		must.NoError(t, state.UpsertNode(structs.MsgTypeTestSetup, uint64(100+i), n))
	}

	job := affinityJob("cache", 2)
	job.TaskGroups[0].Separate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "cache"},
		Scope:    "${meta.zone}",
		Required: true,
	}}
	must.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 200, nil, job))
	running := affinityAlloc(job, nodes[0], 0, 500)
	must.NoError(t, state.UpsertAllocs(structs.MsgTypeTestSetup, 201, []*structs.Allocation{running}))

	tracker := newAllocAffinityTracker(ctx)
	tracker.SetJob(job)
	tg := job.TaskGroups[0]

	tracker.SetTaskGroup(tg)
	ok, _ := tracker.satisfiesRequired(nodes[0])
	must.False(t, ok)
	ok, _ = tracker.satisfiesRequired(nodes[1])
	must.True(t, ok)

	// Stage the stop of the running allocation: zone z0 frees up.
	ctx.Plan().AppendStoppedAlloc(running, "stopping", "", "")
	tracker.SetTaskGroup(tg)
	ok, _ = tracker.satisfiesRequired(nodes[0])
	must.True(t, ok)

	// Place a new allocation in z1: z1 is taken.
	placed := affinityAlloc(job, nodes[1], 1, 500)
	ctx.Plan().AppendAlloc(placed, nil)
	tracker.SetTaskGroup(tg)
	ok, _ = tracker.satisfiesRequired(nodes[1])
	must.False(t, ok)

	// Pop the stop: z0 is taken again.
	ctx.Plan().PopUpdate(running)
	tracker.SetTaskGroup(tg)
	ok, _ = tracker.satisfiesRequired(nodes[0])
	must.False(t, ok)
	must.Eq(t, 2, tracker.current.rules[0].total)
}

// discardPlanner accepts every plan without applying it, so that the same
// evaluation can be processed repeatedly against the same state.
type discardPlanner struct{ h *Harness }

func (p *discardPlanner) SubmitPlan(plan *structs.Plan) (*structs.PlanResult, State, error) {
	return &structs.PlanResult{
		NodeUpdate:      plan.NodeUpdate,
		NodeAllocation:  plan.NodeAllocation,
		NodePreemptions: plan.NodePreemptions,
		AllocIndex:      p.h.NextIndex(),
	}, nil, nil
}
func (p *discardPlanner) UpdateEval(*structs.Evaluation) error  { return nil }
func (p *discardPlanner) CreateEval(*structs.Evaluation) error  { return nil }
func (p *discardPlanner) ReblockEval(*structs.Evaluation) error { return nil }
func (p *discardPlanner) ServersMeetMinimumVersion(*version.Version, bool) bool {
	return true
}

// BenchmarkAllocAffinity measures the latency of scheduling a job with 100
// allocations in a simulated cluster of 10,000 nodes in 100 racks, where 20
// other jobs run 2,000 allocations selected by the rules, with and without
// colocate/separate rules.
func BenchmarkAllocAffinity(b *testing.B) {
	const (
		numNodes  = 10000
		numRacks  = 100
		numJobs   = 20
		perJob    = 100
		jobCount  = 100
		rackScope = "${meta.rack}"
		nodeScope = "${node.unique.id}"
	)

	h := NewHarness(b)
	nodes := make([]*structs.Node, numNodes)
	for i := range nodes {
		node := mock.Node()
		node.Meta["rack"] = fmt.Sprintf("r%d", i%numRacks)
		must.NoError(b, node.ComputeClass())
		must.NoError(b, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
		nodes[i] = node
	}
	for j := 0; j < numJobs; j++ {
		api := affinityJob("api", perJob)
		var on []*structs.Node
		for i := 0; i < perJob; i++ {
			on = append(on, nodes[(j*perJob+i)*7%numNodes])
		}
		upsertRunning(b, h, api, 100, on...)
	}

	cases := []struct {
		name       string
		affinities []*structs.Affinity
		colocate   []*structs.AllocAffinity
		separate   []*structs.AllocAffinity
	}{
		{name: "no rules"},
		{
			// reference: a node affinity widens the node scan like a
			// preferred rule does
			name: "node affinity reference",
			affinities: []*structs.Affinity{{
				LTarget: rackScope, RTarget: "r1", Operand: "=", Weight: 50}},
		},
		{
			name: "separate required self rack",
			separate: []*structs.AllocAffinity{{
				Selector: map[string]string{"app": "cache"}, Scope: rackScope, Required: true}},
		},
		{
			name: "colocate required api rack",
			colocate: []*structs.AllocAffinity{{
				Selector: map[string]string{"app": "api"}, Scope: rackScope, Required: true}},
		},
		{
			name: "colocate preferred api node",
			colocate: []*structs.AllocAffinity{{
				Selector: map[string]string{"app": "api"}, Scope: nodeScope, Weight: 50}},
		},
		{
			name: "all four",
			colocate: []*structs.AllocAffinity{
				{Selector: map[string]string{"app": "api"}, Scope: rackScope, Required: true},
				{Selector: map[string]string{"app": "api"}, Scope: nodeScope, Weight: 50},
			},
			separate: []*structs.AllocAffinity{
				{Selector: map[string]string{"app": "cache"}, Scope: rackScope, Required: true},
				{Selector: map[string]string{"app": "cache"}, Scope: nodeScope, Weight: -50},
			},
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			job := affinityJob("cache", jobCount)
			job.TaskGroups[0].Affinities = tc.affinities
			job.TaskGroups[0].Colocate = tc.colocate
			job.TaskGroups[0].Separate = tc.separate
			must.NoError(b, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
			eval := &structs.Evaluation{
				Namespace:   job.Namespace,
				ID:          uuid.Generate(),
				Priority:    job.Priority,
				TriggeredBy: structs.EvalTriggerJobRegister,
				JobID:       job.ID,
				Status:      structs.EvalStatusPending,
			}
			must.NoError(b, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

			h.Planner = &discardPlanner{h: h}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.Plans = h.Plans[:0]
				must.NoError(b, h.Process(NewServiceScheduler, eval))
			}
			b.StopTimer()

			placed := 0
			for _, allocs := range h.Plans[0].NodeAllocation {
				placed += len(allocs)
			}
			must.Eq(b, jobCount, placed)
		})
	}
}
