// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestPackNodes(t *testing.T) {
	ci.Parallel(t)
	node := NodeSize{CPU: 4000, Memory: 8000, Disk: 10000}

	t.Run("not a division of the sum", func(t *testing.T) {
		res := PackNodes([]Unplaced{{Group: "a", Count: 4, CPU: 100, Memory: 6000, Disk: 1}}, node)
		must.Eq(t, 4, res.Nodes) // 24 GB / 8 GB would say 3
	})
	t.Run("small ones share a node", func(t *testing.T) {
		res := PackNodes([]Unplaced{{Group: "a", Count: 8, CPU: 500, Memory: 1000}}, node)
		must.Eq(t, 1, res.Nodes)
	})
	t.Run("largest first", func(t *testing.T) {
		res := PackNodes([]Unplaced{
			{Group: "small", Count: 2, CPU: 100, Memory: 2000},
			{Group: "big", Count: 2, CPU: 100, Memory: 6000},
		}, node)
		must.Eq(t, 2, res.Nodes) // 6+2 on each
	})
	t.Run("exclusive: one node each, the room is used", func(t *testing.T) {
		res := PackNodes([]Unplaced{
			{Group: "proxy", Count: 3, CPU: 100, Memory: 100, Exclusive: true},
			{Group: "app", Count: 3, CPU: 1000, Memory: 1000},
		}, node)
		must.Eq(t, 3, res.Nodes)
	})
	t.Run("too large for an empty node", func(t *testing.T) {
		res := PackNodes([]Unplaced{
			{Group: "huge", Count: 1, CPU: 100, Memory: 9000},
			{Group: "ok", Count: 1, CPU: 100, Memory: 100},
		}, node)
		must.Eq(t, 1, res.Nodes)
		must.Len(t, 1, res.TooLarge)
		must.Eq(t, "huge", res.TooLarge[0].Group)
	})
	t.Run("nothing", func(t *testing.T) {
		must.Eq(t, 0, PackNodes(nil, node).Nodes)
	})
}
