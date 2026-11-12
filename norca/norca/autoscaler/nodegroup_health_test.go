// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// groupConditions returns the conditions of a node group status.
func groupConditions(conds ...map[string]any) map[string]any {
	list := make([]any, 0, len(conds))
	for _, c := range conds {
		list = append(list, c)
	}
	return map[string]any{"conditions": list}
}

func TestUnableReason(t *testing.T) {
	ci.Parallel(t)

	cond := func(typ, status, reason, message string) map[string]any {
		return map[string]any{"type": typ, "status": status, "reason": reason, "message": message}
	}

	tests := []struct {
		name     string
		status   map[string]any
		expected string
	}{
		{
			name:     "no conditions",
			status:   map[string]any{},
			expected: "",
		},
		{
			name: "everything in order",
			status: groupConditions(
				cond(condProviderAvailable, "True", "Available", ""),
				cond(condProvisioned, "True", "Provisioned", ""),
				cond(condJoinFailing, "False", "Joining", ""),
			),
			expected: "",
		},
		{
			name:     "the provider is gone",
			status:   groupConditions(cond(condProviderAvailable, "False", "ProviderUnavailable", "the heartbeat of provider \"lab\" is 5m0s old")),
			expected: "its provider is not available (the heartbeat of provider \"lab\" is 5m0s old)",
		},
		{
			name:     "nodes fail to join",
			status:   groupConditions(cond(condJoinFailing, "True", "JoinTimeout", "")),
			expected: "its nodes fail to join (JoinTimeout)",
		},
		{
			name:     "nothing to bootstrap a node with",
			status:   groupConditions(cond(condProvisioned, "False", reasonNoBootstrap, "no Variable nodescaler/bootstrap/eu")),
			expected: "its nodes cannot be bootstrapped (no Variable nodescaler/bootstrap/eu)",
		},
		{
			name:     "a create that failed for another reason does not close the group",
			status:   groupConditions(cond(condProvisioned, "False", "CreateFailed", "quota")),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, unableReason(tt.status))
		})
	}
}

func TestDegradedReason(t *testing.T) {
	ci.Parallel(t)

	base := time.Unix(1_700_000_000, 0)
	group := func(spec, ready, pending int64) *demandGroup {
		return &demandGroup{name: "workers-b", spec: spec, ready: ready, pending: pending}
	}

	t.Run("a group with its nodes is not degraded", func(t *testing.T) {
		since := map[string]time.Time{}
		must.Eq(t, "", degradedReason(group(5, 5, 0), since, base))
		must.Eq(t, "", degradedReason(group(5, 3, 0), since, base.Add(time.Hour)))
		must.MapEmpty(t, since)
	})

	t.Run("nodes that are still joining are not missing", func(t *testing.T) {
		since := map[string]time.Time{}
		must.Eq(t, "", degradedReason(group(5, 1, 4), since, base))
		must.Eq(t, "", degradedReason(group(5, 1, 4), since, base.Add(time.Hour)))
		must.MapEmpty(t, since)
	})

	t.Run("an empty group is not degraded", func(t *testing.T) {
		since := map[string]time.Time{}
		must.Eq(t, "", degradedReason(group(0, 0, 0), since, base))
		must.Eq(t, "", degradedReason(group(0, 0, 0), since, base.Add(time.Hour)))
	})

	t.Run("missing nodes count after a while, and no longer when they are back", func(t *testing.T) {
		since := map[string]time.Time{}
		must.Eq(t, "", degradedReason(group(5, 0, 0), since, base))
		must.Eq(t, "", degradedReason(group(5, 2, 0), since, base.Add(unreadyAfter-time.Second)))
		must.Eq(t, "2 of its 5 nodes are ready, for 2m0s", degradedReason(group(5, 2, 0), since, base.Add(unreadyAfter)))

		must.Eq(t, "", degradedReason(group(5, 5, 0), since, base.Add(3*time.Minute)))
		must.Eq(t, "", degradedReason(group(5, 0, 0), since, base.Add(4*time.Minute)))
	})
}

// Blocked work is not given to a node group whose machines cannot be made or
// whose nodes are missing, when another group can take it: the group of a
// datacenter that was lost would otherwise look like the best place to grow.
func TestDemand_UnhealthyGroups(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	// A group of five nodes; lost is one whose nodes are all gone.
	five := func(name, dc string, status map[string]any) testGroup {
		st := map[string]any{"replicas": float64(5)}
		for k, v := range status {
			st[k] = v
		}
		return testGroup{name: name, dc: dc, spec: map[string]any{"replicas": float64(5)}, status: st}
	}
	lost := map[string]any{"replicas": float64(0)}
	noProvider := groupConditions(map[string]any{
		"type": condProviderAvailable, "status": "False", "reason": "ProviderUnavailable",
	})

	t.Run("a group that cannot make machines gets no work", func(t *testing.T) {
		h, objects, events := demandHarness(t,
			[]testGroup{five("workers-a", "dc-a", nil), five("workers-b", "dc-b", noProvider)},
			bigWork(4, "dc-a", "dc-b"))

		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Eq(t, map[string]int64{"workers-a": 9}, scaledTo(objects))
		must.Eq(t, 1, events.countFor(structs.TypeScalingNodeBlocked, "workers-b"))

		// It is told once, not at every look.
		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Eq(t, 1, events.countFor(structs.TypeScalingNodeBlocked, "workers-b"))
	})

	t.Run("a group whose nodes are missing gets no work while another can take it", func(t *testing.T) {
		work := map[[2]string][]Unplaced{}
		h, objects, _ := demandHarness(t,
			[]testGroup{five("workers-a", "dc-a", nil), five("workers-b", "dc-b", lost)}, work)

		// The nodes of dc-b are seen missing, and still are two minutes
		// later, when the work they ran is blocked.
		must.NoError(t, h.runner.scaleForDemand(ctx))
		h.clock.Advance(unreadyAfter)
		for k, v := range bigWork(4, "dc-a", "dc-b") {
			work[k] = v
		}

		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Eq(t, map[string]int64{"workers-a": 9}, scaledTo(objects))
	})

	t.Run("the only group there is still grows when its nodes are missing", func(t *testing.T) {
		work := map[[2]string][]Unplaced{}
		h, objects, _ := demandHarness(t, []testGroup{five("workers-b", "dc-b", lost)}, work)

		must.NoError(t, h.runner.scaleForDemand(ctx))
		h.clock.Advance(unreadyAfter)
		for k, v := range bigWork(4, "dc-b") {
			work[k] = v
		}

		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Eq(t, map[string]int64{"workers-b": 9}, scaledTo(objects))
	})

	t.Run("the only group there is gets nothing when it cannot make machines", func(t *testing.T) {
		h, objects, events := demandHarness(t,
			[]testGroup{five("workers-b", "dc-b", noProvider)}, bigWork(4, "dc-b"))

		must.NoError(t, h.runner.scaleForDemand(ctx))
		must.Len(t, 0, objects.requests())
		must.Positive(t, events.count(structs.TypeScalingNodeBlocked))
	})
}
