// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"math"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// countPolicy is a policy with one target-value check of the fake source.
func countPolicy(cfg map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"evaluation_interval": "10s",
		"cooldown":            "1m",
		"check": []interface{}{map[string]interface{}{
			"cpu": []interface{}{map[string]interface{}{
				"source": "fake",
				"query":  "cpu",
				"strategy": []interface{}{map[string]interface{}{
					"target-value": []interface{}{cfg},
				}},
			}},
		}},
	}
}

// requestedCounts are the counts the scaler was asked for; a dry run (no
// count) is -1.
func requestedCounts(h *harness) []int64 {
	var out []int64
	for _, r := range h.scaler.requests() {
		if r.Count == nil {
			out = append(out, -1)
		} else {
			out = append(out, *r.Count)
		}
	}
	return out
}

// An infinite metric (a ratio over zero) is a failed query: it does not scale
// the group to its minimum (review finding ASP-1).
func TestStrategyCount_InfiniteMetricDoesNotScale(t *testing.T) {
	ci.Parallel(t)

	for _, v := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
		h.apm.value = v
		_ = h.evaluate()
		must.SliceEmpty(t, requestedCounts(h), must.Sprintf("value %v", v))
	}
}

// A max_scale_up at the end of the range of an int64 does not wrap around
// into a negative count (review finding ASP-1).
func TestStrategyCount_MaxScaleUpDoesNotOverflow(t *testing.T) {
	ci.Parallel(t)

	doc := countPolicy(map[string]interface{}{"target": 50, "max_scale_up": "9223372036854775807"})
	vcfg := DefaultConfig()
	vcfg.APMs = []*PluginConfig{{Name: "fake", Driver: plugins.InternalAPMPrometheus}}
	must.NoError(t, ValidatePolicy(&structs.ScalingPolicy{Policy: doc, Min: 1, Max: 10}, vcfg))
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, doc, nil)
	h.apm.value = 100 // twice the target on 2 instances: 4
	must.NoError(t, h.evaluate())
	must.Eq(t, []int64{4}, requestedCounts(h))
}

// A strategy that computes -1 scales to the minimum; it is not taken for a dry
// run, which is marked in the meta of the action (review finding ASP-3).
func TestStrategyCount_MinusOneIsNotADryRun(t *testing.T) {
	ci.Parallel(t)

	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, targetValuePolicy(nil), nil)
	h.apm.value = -25 // factor -0.5 on 2 instances: ceil(-1) = -1
	must.NoError(t, h.evaluate())
	must.Eq(t, []int64{1}, requestedCounts(h)) // the minimum of the policy
	job, err := h.store.JobByID(nil, h.job.Namespace, h.job.ID)
	must.NoError(t, err)
	must.Eq(t, 1, job.TaskGroups[0].Count)
}
