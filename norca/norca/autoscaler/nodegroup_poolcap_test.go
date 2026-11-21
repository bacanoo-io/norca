// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// The checks of a node group stay within the limits of its pool, max_nodes and
// max_step, as the blocked work does (review finding ASC-3: with max_nodes 3
// and max_step 1 a check raised a group of 2 to 4).
func TestNodeGroupCheck_PoolLimits(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name string
		pool *NodePoolScaling
		want int64
	}{
		{"max_nodes and max_step", &NodePoolScaling{MaxNodes: 3, MaxStep: 1}, 3},
		{"max_step", &NodePoolScaling{MaxStep: 1}, 3},
		{"max_nodes", &NodePoolScaling{MaxNodes: 3}, 3},
		{"no limit", &NodePoolScaling{}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := nodeGroupSpec() // replicas 2, max 10, target 50, fake metric 100: wants 4
			spec["enforce"] = true
			h, objects := nodeGroupHarness(t, spec)
			h.cfg.NodeScaling = &NodeScalingConfig{Pools: map[string]*NodePoolScaling{"workers": tc.pool}}
			h.newRunner()
			must.NoError(t, evaluateNodeGroup(t, h))
			reqs := objects.requests()
			must.Len(t, 1, reqs)
			must.Eq(t, tc.want, reqs[0].Replicas)
		})
	}
}
