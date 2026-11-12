// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
)

func TestCheckRunner_Restate(t *testing.T) {
	ci.Parallel(t)

	now := time.Unix(1_700_000_000, 0)
	// average answers as a group that ran 5 instances on average.
	average := func(avg float64, ok bool) func(time.Time, time.Time) (float64, bool) {
		return func(_, _ time.Time) (float64, bool) { return avg, ok }
	}

	tests := []struct {
		name       string
		scope      valueScope
		pop        population
		input      float64
		expected   float64
		expectedOK bool
	}{
		{
			name:       "a settled group keeps its value",
			scope:      valueScopeRunning,
			pop:        population{desired: 6, running: 6},
			input:      60,
			expected:   60,
			expectedOK: true,
		},
		{
			name:       "measured on the 4 that run of 6",
			scope:      valueScopeRunning,
			pop:        population{desired: 6, running: 4},
			input:      75,
			expected:   50,
			expectedOK: true,
		},
		{
			name:       "more run than asked for",
			scope:      valueScopeRunning,
			pop:        population{desired: 4, running: 6},
			input:      50,
			expected:   75,
			expectedOK: true,
		},
		{
			name:       "a window value is laid on the average of the window",
			scope:      valueScopeWindow,
			pop:        population{desired: 6, running: 4, average: average(5, true)},
			input:      60,
			expected:   50,
			expectedOK: true,
		},
		{
			name:       "a window value falls back to what runs when the average is unknown",
			scope:      valueScopeWindow,
			pop:        population{desired: 6, running: 4, average: average(0, false)},
			input:      75,
			expected:   50,
			expectedOK: true,
		},
		{
			name:       "a running value does not ask for the average",
			scope:      valueScopeRunning,
			pop:        population{desired: 6, running: 6, average: average(3, true)},
			input:      60,
			expected:   60,
			expectedOK: true,
		},
		{
			name:       "a value of unknown scope is used as it is",
			scope:      valueScopeUnknown,
			pop:        population{desired: 6, running: 4},
			input:      75,
			expected:   75,
			expectedOK: true,
		},
		{
			name:       "nothing asked for: used as it is",
			scope:      valueScopeRunning,
			pop:        population{desired: 0, running: 0},
			input:      75,
			expected:   75,
			expectedOK: true,
		},
		{
			name:       "nothing runs to measure a value per instance on",
			scope:      valueScopeRunning,
			pop:        population{desired: 6, running: 0},
			input:      75,
			expectedOK: false,
		},
		{
			name:       "nothing ran in the window",
			scope:      valueScopeWindow,
			pop:        population{desired: 6, running: 2, average: average(0, true)},
			input:      75,
			expectedOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := newCheckRunner(&CheckRunnerConfig{
				Log:    hclog.NewNullLogger(),
				Policy: &sdk.ScalingPolicy{},
				Scope:  tt.scope,
				Now:    func() time.Time { return now },
			}, &sdk.ScalingPolicyCheck{QueryWindow: time.Minute})

			input := sdk.TimestampedMetrics{{Timestamp: now, Value: tt.input}}
			got, ok := ch.restate(tt.pop, input)
			must.Eq(t, tt.expectedOK, ok)
			if !tt.expectedOK {
				return
			}
			must.Len(t, 1, got)
			must.InDelta(t, tt.expected, got[0].Value, 1e-6)
			// The input is never changed: it is shared through the cache.
			must.Eq(t, tt.input, input[0].Value)
		})
	}
}
