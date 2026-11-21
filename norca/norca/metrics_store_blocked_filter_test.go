// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// The scheduler blocks an evaluation for any placement that failed. Work that
// every node was filtered out for, by a check a new node would fail too, is
// not demand for more nodes (review finding ASC-1).
func TestFilteredForNewNodes(t *testing.T) {
	ci.Parallel(t)

	job := mock.Job()
	tg := job.TaskGroups[0]
	tg.Constraints = append(tg.Constraints, &structs.Constraint{LTarget: "${node.class}", Operand: "=", RTarget: "gpu"})

	cases := []struct {
		name   string
		metric structs.AllocMetric
		want   string // a part of the reason; "" for none
	}{
		{"no node to try", structs.AllocMetric{NodesEvaluated: 0}, ""},
		{"a node passed and was short of resources", structs.AllocMetric{
			NodesEvaluated: 3, NodesFiltered: 2, NodesExhausted: 1,
			ConstraintFiltered: map[string]int{"missing drivers": 2},
		}, ""},
		{"distinct_hosts", structs.AllocMetric{
			NodesEvaluated: 3, NodesFiltered: 3, ConstraintFiltered: map[string]int{structs.ConstraintDistinctHosts: 3},
		}, ""},
		{"distinct_property and a class constraint of the job", structs.AllocMetric{
			NodesEvaluated: 3, NodesFiltered: 3, ConstraintFiltered: map[string]int{
				"distinct_property: ${meta.rack}=r1 used by 1 allocs": 1,
				"${node.class} = gpu":   1,
				computedClassIneligible: 1,
			},
		}, ""},
		{"a meta constraint", structs.AllocMetric{
			NodesEvaluated: 3, NodesFiltered: 3, ConstraintFiltered: map[string]int{"${meta.rack} = r9": 3},
		}, "${meta.rack} = r9"},
		{"a missing driver next to distinct_hosts", structs.AllocMetric{
			NodesEvaluated: 3, NodesFiltered: 3, ConstraintFiltered: map[string]int{
				structs.ConstraintDistinctHosts: 1, "missing drivers": 2,
			},
		}, "missing drivers"},
		{"missing devices", structs.AllocMetric{
			NodesEvaluated: 1, NodesFiltered: 1, ConstraintFiltered: map[string]int{"missing devices": 1},
		}, "missing devices"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filteredForNewNodes(job, tg, &tc.metric)
			if tc.want == "" {
				must.Eq(t, "", got)
			} else {
				must.StrContains(t, got, tc.want)
			}
		})
	}
}

func TestIneligibleNodeGroups(t *testing.T) {
	ci.Parallel(t)

	classes := map[string][]string{
		"gpu-pool":   {"v1:1"},
		"mixed":      {"v1:1", "v1:2"},
		"cpu-pool":   {"v1:2"},
		"unknown":    {"v1:3"},
		"no-classes": nil,
	}
	eval := &structs.Evaluation{ClassEligibility: map[string]bool{"v1:1": false, "v1:2": true}}
	must.Eq(t, []string{"gpu-pool"}, ineligibleNodeGroups(eval, classes))

	escaped := eval.Copy()
	escaped.EscapedComputedClass = true
	must.SliceEmpty(t, ineligibleNodeGroups(escaped, classes))
	must.SliceEmpty(t, ineligibleNodeGroups(&structs.Evaluation{}, classes))
}

// A job that no node can take for a reason other than its size is blocked by
// the scheduler like work that waits for capacity; the blocked work it gives
// node scaling is unverifiable, and the node groups whose nodes the scheduler
// found ineligible are named. Work that waits for capacity stays demand.
func TestCollectBlocked_FilteredWorkIsNotDemand(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	st := s.fsm.State()

	for i := 0; i < 3; i++ {
		node := mock.Node()
		node.Meta[structs.NodeMetaNodeGroup] = "workers"
		node.ComputeClass()
		must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, uint64(1000+i), node))
	}

	cases := []struct {
		name       string
		mutate     func(*structs.Job)
		unverified string // a part of the reason; "" for demand
		ineligible bool   // the group is named ineligible
		cores      int    // the cores one allocation reserves
	}{
		{name: "attribute constraint", mutate: func(j *structs.Job) {
			j.Constraints = []*structs.Constraint{{LTarget: "${attr.kernel.name}", RTarget: "windows", Operand: "="}}
		}, unverified: "${attr.kernel.name} = windows", ineligible: true},
		{name: "meta constraint", mutate: func(j *structs.Job) {
			j.TaskGroups[0].Constraints = append(j.TaskGroups[0].Constraints,
				&structs.Constraint{LTarget: "${meta.rack}", RTarget: "r9", Operand: "="})
		}, unverified: "${meta.rack} = r9", ineligible: true},
		{name: "missing driver", mutate: func(j *structs.Job) {
			j.TaskGroups[0].Tasks[0].Driver = "qemu"
		}, unverified: "missing drivers", ineligible: true},
		{name: "device", mutate: func(j *structs.Job) {
			j.TaskGroups[0].Tasks[0].Resources.Devices = []*structs.RequestedDevice{{Name: "nvidia/gpu", Count: 1}}
		}, unverified: "missing devices", ineligible: true},
		{name: "too large for the free resources", mutate: func(j *structs.Job) {
			j.TaskGroups[0].Tasks[0].Resources.CPU = 5000
		}},
		// The cores are part of the size of the work (review finding
		// ASC-5).
		{name: "more cores than a node has", mutate: func(j *structs.Job) {
			j.TaskGroups[0].Tasks[0].Resources.CPU = 0
			j.TaskGroups[0].Tasks[0].Resources.Cores = 64
		}, cores: 64},
	}

	for _, tc := range cases {
		job := mock.Job()
		job.TaskGroups[0].Count = 1
		job.TaskGroups[0].Tasks[0].Services = nil
		tc.mutate(job)
		req := &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}
		var resp structs.JobRegisterResponse
		must.NoError(t, s.RPC("Job.Register", req, &resp), must.Sprint(tc.name))

		testutil.WaitForResult(func() (bool, error) {
			for _, e := range s.blockedEvals.Evals() {
				if e.JobID == job.ID {
					return true, nil
				}
			}
			return false, fmt.Errorf("no blocked evaluation yet for %s", tc.name)
		}, func(err error) { t.Fatalf("%s: %v", tc.name, err) })

		snap, err := st.Snapshot()
		must.NoError(t, err)
		d := collectBlocked(snap, s.blockedEvals.Evals())[[2]string{"dc1", "default"}]
		must.NotNil(t, d, must.Sprint(tc.name))
		found := false
		for _, u := range d.unplaced {
			if u.Job != job.ID {
				continue
			}
			found = true
			must.Eq(t, resp.JobModifyIndex, u.JobIndex, must.Sprint(tc.name))
			must.Eq(t, tc.cores, u.Cores, must.Sprint(tc.name))
			if tc.unverified == "" {
				must.Eq(t, "", u.Unverifiable, must.Sprint(tc.name))
			} else {
				must.True(t, strings.Contains(u.Unverifiable, tc.unverified),
					must.Sprintf("%s: unverifiable %q", tc.name, u.Unverifiable))
			}
			if tc.ineligible {
				must.Eq(t, []string{"workers"}, u.IneligibleGroups, must.Sprint(tc.name))
			} else {
				must.SliceEmpty(t, u.IneligibleGroups, must.Sprint(tc.name))
			}
		}
		must.True(t, found, must.Sprint(tc.name))
	}
}
