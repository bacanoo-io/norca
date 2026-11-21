// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package targetvalue

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/shoenig/test/must"
)

// Values that would turn the scaling around are refused at admission and
// when the strategy runs (review finding ASP-9).
func TestValidateConfig_ReversingValues(t *testing.T) {
	for _, cfg := range []map[string]string{
		{"target": "-50"},
		{"target": "NaN"},
		{"target": "+Inf"},
		{"target": "50", "threshold": "-0.1"},
		{"target": "50", "max_scale_up": "-2"},
		{"target": "50", "max_scale_down": "-2"},
	} {
		must.Error(t, ValidateConfig(cfg), must.Sprintf("%v", cfg))

		eval := &sdk.ScalingCheckEvaluation{
			Check:   &sdk.ScalingPolicyCheck{Strategy: &sdk.ScalingPolicyStrategy{Config: cfg}},
			Metrics: sdk.TimestampedMetrics{{Value: 100}},
			Action:  &sdk.ScalingAction{},
		}
		_, err := (&StrategyPlugin{}).Run(eval, 8)
		must.Error(t, err, must.Sprintf("run with %v", cfg))
	}
	for _, cfg := range []map[string]string{
		{"target": "0"},
		{"target": "50", "threshold": "0.05", "max_scale_up": "0", "max_scale_down": "3"},
	} {
		must.NoError(t, ValidateConfig(cfg), must.Sprintf("%v", cfg))
	}
}
