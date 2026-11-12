// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestTargetState_CooldownUntil(t *testing.T) {
	ci.Parallel(t)

	const (
		cooldown   = 5 * time.Minute
		cooldownUp = 20 * time.Second
	)
	policy := &sdk.ScalingPolicy{Cooldown: cooldown, CooldownOnScaleUp: cooldownUp}

	base := time.Unix(1_700_000_000, 0)
	at := func(d time.Duration) int64 { return base.Add(d).UnixNano() }
	scaled := func(d time.Duration, prev, count int64) *structs.ScalingEvent {
		return &structs.ScalingEvent{Time: at(d), PreviousCount: prev, Count: pointer.Of(count)}
	}

	// Events are stored newest first.
	tests := []struct {
		name     string
		events   []*structs.ScalingEvent
		expected time.Time
	}{
		{
			name:     "no events",
			events:   nil,
			expected: time.Time{},
		},
		{
			name:     "scale up",
			events:   []*structs.ScalingEvent{scaled(0, 4, 6)},
			expected: base.Add(cooldownUp),
		},
		{
			name:     "scale down",
			events:   []*structs.ScalingEvent{scaled(0, 6, 4)},
			expected: base.Add(cooldown),
		},
		{
			name: "unchanged count after a scale up keeps the scale up cooldown",
			events: []*structs.ScalingEvent{
				scaled(30*time.Second, 6, 6),
				scaled(0, 4, 6),
			},
			expected: base.Add(cooldownUp),
		},
		{
			name:     "only an unchanged count",
			events:   []*structs.ScalingEvent{scaled(0, 6, 6)},
			expected: time.Time{},
		},
		{
			name: "informational and failed events are skipped",
			events: []*structs.ScalingEvent{
				{Time: at(2 * time.Minute), PreviousCount: 6},
				{Time: at(time.Minute), PreviousCount: 6, Count: pointer.Of(int64(8)), Error: true},
				scaled(0, 6, 4),
			},
			expected: base.Add(cooldown),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := &targetState{events: tt.events}
			must.Eq(t, tt.expected, ts.cooldownUntil(policy))
		})
	}
}
