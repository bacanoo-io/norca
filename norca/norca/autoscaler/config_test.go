// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestBoundsOnly(t *testing.T) {
	ci.Parallel(t)

	check := &structs.ScalingCheck{Name: "cpu", Metric: "norca.cpu", Strategy: "target-value"}
	cases := []struct {
		name     string
		policy   *structs.ScalingPolicy
		expected bool
	}{
		{name: "no policy", policy: nil, expected: false},
		{name: "bounds only", policy: &structs.ScalingPolicy{Min: 1, Max: 3}, expected: true},
		{
			name:     "a document without a check",
			policy:   &structs.ScalingPolicy{Policy: map[string]any{"cooldown": "2m"}},
			expected: true,
		},
		{
			name:     "a typed check",
			policy:   &structs.ScalingPolicy{Checks: []*structs.ScalingCheck{check}},
			expected: false,
		},
		{
			name:     "a check in the document",
			policy:   &structs.ScalingPolicy{Policy: map[string]any{"check": []any{}}},
			expected: false,
		},
		{
			name:     "a target block",
			policy:   &structs.ScalingPolicy{Policy: map[string]any{"target": []any{}}},
			expected: false,
		},
		{
			name: "a node group",
			policy: &structs.ScalingPolicy{
				Target: map[string]string{structs.ScalingTargetNodeGroup: "workers"},
			},
			expected: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must.Eq(t, tc.expected, BoundsOnly(tc.policy))
		})
	}
}
