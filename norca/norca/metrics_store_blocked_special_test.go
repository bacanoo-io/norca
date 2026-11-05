// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// TestBlockedDemand_SpecialCases checks what the blocked demand leaves out and
// how the allocations of a job count against a distinct_property.
func TestBlockedDemand_SpecialCases(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableMetricsStore(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	st := s.fsm.State()

	node := mock.Node()
	node.NodePool = "default"
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	index := uint64(1001)
	newJob := func(mutate func(*structs.Job)) *structs.Job {
		job := mock.Job()
		job.NodePool = "default"
		if mutate != nil {
			mutate(job)
		}
		index++
		must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, index, nil, job))
		return job
	}
	block := func(job *structs.Job, metric *structs.AllocMetric, mutate func(*structs.Evaluation)) {
		eval := mock.BlockedEval()
		eval.JobID, eval.Namespace = job.ID, job.Namespace
		if mutate != nil {
			mutate(eval)
		}
		eval.FailedTGAllocs = map[string]*structs.AllocMetric{job.TaskGroups[0].Name: metric}
		s.blockedEvals.Block(eval)
	}
	here := func() *structs.AllocMetric {
		return &structs.AllocMetric{NodesAvailable: map[string]int{node.Datacenter: 1}}
	}
	demand := func() map[[2]string]*blockedDemand {
		snap, err := st.Snapshot()
		must.NoError(t, err)
		return collectBlocked(snap, s.blockedEvals.Evals())
	}

	// A stopped job, a purged job, a system evaluation and one that waits
	// for a quota are not demand.
	stopped := newJob(func(j *structs.Job) { j.Stop = true })
	block(stopped, here(), nil)
	purged := newJob(nil)
	block(purged, here(), nil)
	index++
	must.NoError(t, st.DeleteJob(index, purged.Namespace, purged.ID))
	system := newJob(func(j *structs.Job) { j.Type = structs.JobTypeSystem })
	block(system, here(), func(e *structs.Evaluation) { e.Type = structs.JobTypeSystem })
	quota := newJob(nil)
	block(quota, here(), func(e *structs.Evaluation) { e.QuotaLimitReached = "q" })
	must.MapEmpty(t, demand())

	// A job with a wildcard datacenter and no node to name: nowhere to put it.
	wild := newJob(func(j *structs.Job) { j.Datacenters = []string{"dc*"} })
	block(wild, &structs.AllocMetric{}, nil)
	must.MapEmpty(t, demand())

	// Without a node pool the job is in "default"; named datacenters are
	// used when no node is there to look at.
	named := newJob(func(j *structs.Job) { j.NodePool = ""; j.Datacenters = []string{"dc-x", "dc-y"} })
	block(named, &structs.AllocMetric{CoalescedFailures: 1}, nil)
	d := demand()
	must.MapLen(t, 2, d)
	for _, dc := range []string{"dc-x", "dc-y"} {
		got := d[[2]string{dc, "default"}]
		must.NotNil(t, got)
		must.Eq(t, 2, got.unplaced[0].Count) // one failed and one coalesced
	}

	// distinct_property: a constraint of the group counts the allocations of
	// the group, one of the job those of the whole job, and a finished
	// allocation counts for nothing.
	perGroup := func(onJob bool) *structs.Job {
		return newJob(func(j *structs.Job) {
			j.TaskGroups = append(j.TaskGroups, j.TaskGroups[0].Copy())
			j.TaskGroups[1].Name = "other"
			c := &structs.Constraint{Operand: structs.ConstraintDistinctProperty, LTarget: "${node.datacenter}", RTarget: "5"}
			if onJob {
				j.Constraints = append(j.Constraints, c)
			} else {
				j.TaskGroups[0].Constraints = append(j.TaskGroups[0].Constraints, c)
			}
		})
	}
	alloc := func(job *structs.Job, group string, status string) {
		a := mock.Alloc()
		a.Job, a.JobID, a.Namespace, a.TaskGroup, a.NodeID = job, job.ID, job.Namespace, group, node.ID
		a.ClientStatus = status
		index++
		must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, index, []*structs.Allocation{a}))
	}
	used := func(job *structs.Job) int {
		snap, err := st.Snapshot()
		must.NoError(t, err)
		props, unverifiable := distinctProperties(snap, job, job.TaskGroups[0])
		must.Eq(t, "", unverifiable)
		must.Len(t, 1, props)
		return props[0].Used[node.Datacenter]
	}

	group := perGroup(false)
	alloc(group, group.TaskGroups[0].Name, structs.AllocClientStatusRunning)
	alloc(group, "other", structs.AllocClientStatusRunning)
	alloc(group, group.TaskGroups[0].Name, structs.AllocClientStatusComplete)
	must.Eq(t, 1, used(group)) // only the running allocation of the group

	job := perGroup(true)
	alloc(job, job.TaskGroups[0].Name, structs.AllocClientStatusRunning)
	alloc(job, "other", structs.AllocClientStatusRunning)
	must.Eq(t, 2, used(job)) // both groups of the job

	// The limit is the value of the constraint.
	snap, err := st.Snapshot()
	must.NoError(t, err)
	props, _ := distinctProperties(snap, job, job.TaskGroups[0])
	must.Eq(t, 5, props[0].Limit)
}

// TestClassConstraints checks the constraints on the node class a blocked
// task group carries: the job's, the group's and its tasks'.
func TestClassConstraints(t *testing.T) {
	ci.Parallel(t)

	job := mock.Job()
	tg := job.TaskGroups[0]
	job.Constraints = []*structs.Constraint{{LTarget: "${node.class}", Operand: "=", RTarget: "gpu"}}
	tg.Constraints = []*structs.Constraint{{LTarget: "${node.class}", Operand: "regexp", RTarget: "^g"}}
	tg.Tasks[0].Constraints = []*structs.Constraint{{LTarget: "${attr.kernel.name}", Operand: "=", RTarget: "linux"}}

	var u autoscaler.Unplaced
	got := classConstraints(job, tg, &u)
	must.Eq(t, []autoscaler.ClassConstraint{{Operand: "=", Value: "gpu"}, {Operand: "regexp", Value: "^g"}}, got)
	must.Eq(t, "", u.Unverifiable)

	// An operand that can not be checked ahead of time: the work waits.
	tg.Constraints = []*structs.Constraint{{LTarget: "${node.class}", Operand: "version", RTarget: ">= 1"}}
	u = autoscaler.Unplaced{}
	got = classConstraints(job, tg, &u)
	must.Len(t, 1, got)
	must.NotEq(t, "", u.Unverifiable)
}
