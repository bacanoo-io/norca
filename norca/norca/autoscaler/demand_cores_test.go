// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// Blocked work that reserves cores is sized with the MHz of a core of the
// group's nodes (review finding ASC-5: its CPU was 0, and 8 allocations of 2
// cores asked for one node of 4 cores).
func TestDemand_ReservedCores(t *testing.T) {
	ci.Parallel(t)

	work := map[[2]string][]Unplaced{{"dc-a", "workers"}: {{
		Namespace: "default", Job: "pinned", Group: "g", Count: 8, Cores: 2, Memory: 512,
	}}}
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, work)

	// A node of the group: 4 cores of 1000 MHz.
	node := mock.Node()
	node.Meta[structs.NodeMetaNodeGroup] = "workers-a"
	node.NodeResources.Cpu.CpuShares = 4000
	node.NodeResources.Cpu.TotalCpuCores = 4
	must.NoError(t, h.store.UpsertNode(structs.MsgTypeTestSetup, 4100, node))
	h.newRunner()

	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	// Two allocations of 2 cores fit a node of 4: four new nodes.
	must.Eq(t, map[string]int64{"workers-a": 5}, scaledTo(objects))
}

// Without a node of the group to tell the MHz of a core, work that reserves
// cores does not grow the group on a guess.
func TestDemand_ReservedCoresUnknownMHz(t *testing.T) {
	ci.Parallel(t)

	work := map[[2]string][]Unplaced{{"dc-a", "workers"}: {{
		Namespace: "default", Job: "pinned", Group: "g", Count: 8, Cores: 2, Memory: 512,
	}}}
	h, objects, _ := demandHarness(t, []testGroup{{name: "workers-a", dc: "dc-a"}}, work)
	must.NoError(t, h.runner.scaleForDemand(context.Background()))
	must.MapEmpty(t, scaledTo(objects))
	must.StrContains(t, h.runner.capacity.unhelped(targetKey{namespace: "default", job: "pinned", group: "g"}),
		"the MHz of a core")
}
