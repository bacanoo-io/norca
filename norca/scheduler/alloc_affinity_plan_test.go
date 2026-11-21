// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// The plan applier checks the required colocate and separate rules again on
// its state: a placement that a job placed meanwhile forbids is found
// (review finding SCH-6).
func TestAllocAffinityViolations(t *testing.T) {
	ci.Parallel(t)

	h := NewHarness(t)
	zone := func(z string) *structs.Node {
		n := mock.Node()
		n.Meta["zone"] = z
		must.NoError(t, n.ComputeClass())
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), n))
		return n
	}
	z0, z1 := zone("z0"), zone("z1")

	// Job x runs in z0.
	x := mock.Job()
	x.Meta = map[string]string{"app": "x"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, x))
	ax := mock.Alloc()
	ax.Job, ax.JobID, ax.NodeID, ax.Namespace = x, x.ID, z0.ID, x.Namespace
	ax.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{ax}))

	// Job y must not share a zone with x.
	y := mock.Job()
	y.Meta = map[string]string{"app": "y"}
	y.TaskGroups[0].Separate = []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "x"}, Scope: "${meta.zone}", Required: true,
	}}
	plan := func(node *structs.Node) *structs.Plan {
		a := mock.Alloc()
		a.Job, a.JobID, a.NodeID, a.Namespace, a.TaskGroup = nil, y.ID, node.ID, y.Namespace, y.TaskGroups[0].Name
		return &structs.Plan{Job: y, NodeAllocation: map[string][]*structs.Allocation{node.ID: {a}}}
	}

	got, err := AllocAffinityViolations(h.State, plan(z0), testlog.HCLogger(t))
	must.NoError(t, err)
	must.Len(t, 1, got)
	must.StrContains(t, got[0], "separate")

	got, err = AllocAffinityViolations(h.State, plan(z1), testlog.HCLogger(t))
	must.NoError(t, err)
	must.SliceEmpty(t, got)

	// A job without a required rule is not looked at.
	free := mock.Job()
	a := mock.Alloc()
	a.NodeID = z0.ID
	got, err = AllocAffinityViolations(h.State, &structs.Plan{Job: free,
		NodeAllocation: map[string][]*structs.Allocation{z0.ID: {a}}}, testlog.HCLogger(t))
	must.NoError(t, err)
	must.SliceEmpty(t, got)
}
