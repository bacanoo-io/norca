// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestScalingPolicy_FillTypedPolicy(t *testing.T) {
	ci.Parallel(t)

	// The document as the HCL parsers produce it, with both check forms.
	p := &ScalingPolicy{
		Policy: map[string]interface{}{
			"evaluation_interval":     "30s",
			"cooldown":                "2m",
			"cooldown_on_scale_up":    "1m",
			"on_check_error":          "ignore",
			"scale_during_deployment": "up_only",
			"foo":                     "bar",
			"check": []map[string]interface{}{
				{"cpu": []map[string]interface{}{{
					"metric":   "nomad.cpu",
					"strategy": "target-value",
					"target":   70,
				}}},
				{"latency": []map[string]interface{}{{
					"source":              "prometheus",
					"query":               "q",
					"query_window":        "5m",
					"query_window_offset": "1m",
					"group":               "g",
					"on_error":            "fail",
					"strategy": []map[string]interface{}{{
						"threshold": []map[string]interface{}{{"upper_bound": 0.3, "delta": 1}},
					}},
				}}},
			},
		},
	}
	must.NoError(t, p.FillTypedPolicy())
	must.True(t, p.HasTypedPolicy())
	must.Eq(t, 30*time.Second, p.EvaluationInterval)
	must.Eq(t, 2*time.Minute, p.Cooldown)
	must.Eq(t, time.Minute, p.CooldownOnScaleUp)
	must.Eq(t, ScalingOnErrorIgnore, p.OnCheckError)
	must.Eq(t, ScaleDuringDeploymentUpOnly, p.ScaleDuringDeployment)
	must.Eq(t, []*ScalingCheck{
		{
			Name:           "cpu",
			Metric:         "nomad.cpu",
			Strategy:       "target-value",
			StrategyConfig: map[string]string{"target": "70"},
		},
		{
			Name:              "latency",
			Source:            "prometheus",
			Query:             "q",
			QueryWindow:       5 * time.Minute,
			QueryWindowOffset: time.Minute,
			Group:             "g",
			OnError:           ScalingOnErrorFail,
			Strategy:          "threshold",
			StrategyConfig:    map[string]string{"upper_bound": "0.3", "delta": "1"},
		},
	}, p.Checks)

	// The typed policy is not replaced once it is set.
	p.Policy["evaluation_interval"] = "1h"
	must.NoError(t, p.FillTypedPolicy())
	must.Eq(t, 30*time.Second, p.EvaluationInterval)
}

func TestScalingPolicy_FillTypedPolicy_Decoded(t *testing.T) {
	ci.Parallel(t)

	// The document after JSON or msgpack decoding, with an instant query.
	p := &ScalingPolicy{
		Policy: map[string]interface{}{
			"check": []interface{}{
				map[string]interface{}{"cpu": []interface{}{
					map[string]interface{}{
						"query":        "avg_cpu",
						"query_window": "instant",
						"strategy": []interface{}{map[string]interface{}{
							"fixed-value": []interface{}{map[string]interface{}{"value": float64(3)}},
						}},
					},
				}},
			},
		},
	}
	must.NoError(t, p.FillTypedPolicy())
	must.Len(t, 1, p.Checks)
	must.True(t, p.Checks[0].QueryInstant)
	must.Eq(t, "fixed-value", p.Checks[0].Strategy)
	must.Eq(t, map[string]string{"value": "3"}, p.Checks[0].StrategyConfig)
}

func TestScalingPolicy_FillTypedPolicy_Errors(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name string
		doc  map[string]interface{}
		err  string
	}{
		{"bad duration", map[string]interface{}{"cooldown": "soon"}, "policy.cooldown"},
		{"duration type", map[string]interface{}{"evaluation_interval": 10}, "policy.evaluation_interval"},
		{"on_check_error type", map[string]interface{}{"on_check_error": true}, "policy.on_check_error"},
		{"checks shape", map[string]interface{}{"check": "cpu"}, "policy.check"},
		{"bad query window", map[string]interface{}{"check": []interface{}{
			map[string]interface{}{"cpu": map[string]interface{}{"query_window": "x", "strategy": "s"}},
		}}, "policy.check[cpu].query_window"},
		{"param outside block", map[string]interface{}{"check": []interface{}{
			map[string]interface{}{"cpu": map[string]interface{}{
				"target":   1,
				"strategy": []interface{}{map[string]interface{}{"target-value": map[string]interface{}{}}},
			}},
		}}, "strategy parameters must be inside the strategy block"},
		{"two strategies", map[string]interface{}{"check": []interface{}{
			map[string]interface{}{"cpu": map[string]interface{}{
				"strategy": []interface{}{
					map[string]interface{}{"a": map[string]interface{}{}},
					map[string]interface{}{"b": map[string]interface{}{}},
				},
			}},
		}}, "expected 1 strategy block"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ScalingPolicy{Policy: tc.doc}
			must.ErrorContains(t, p.FillTypedPolicy(), tc.err)
		})
	}
}

func TestScalingPolicy_TypedCopyEqual(t *testing.T) {
	ci.Parallel(t)

	p := &ScalingPolicy{
		ID:                    "id",
		Policy:                map[string]interface{}{"a": "b"},
		Target:                map[string]string{},
		EvaluationInterval:    time.Minute,
		Cooldown:              time.Minute,
		CooldownOnScaleUp:     time.Second,
		OnCheckError:          ScalingOnErrorFail,
		ScaleDuringDeployment: ScaleDuringDeploymentUpOnly,
		Checks: []*ScalingCheck{{
			Name:           "cpu",
			Metric:         "nomad.cpu",
			Strategy:       "target-value",
			StrategyConfig: map[string]string{"target": "70"},
		}},
	}
	c := p.Copy()
	must.Eq(t, p, c)
	must.True(t, p.TypedPolicyEqual(c))
	must.False(t, p.Diff(c))

	c.Checks[0].StrategyConfig["target"] = "80"
	must.Eq(t, "70", p.Checks[0].StrategyConfig["target"])
	must.False(t, p.TypedPolicyEqual(c))
	must.True(t, p.Diff(c))
	must.False(t, p.Checks[0].Equal(c.Checks[0]))

	c = p.Copy()
	c.ScaleDuringDeployment = ""
	must.False(t, p.TypedPolicyEqual(c))
}

func TestScalingPolicy_TypedDiff(t *testing.T) {
	ci.Parallel(t)

	old := &ScalingPolicy{
		Min: 1, Max: 5,
		EvaluationInterval: time.Minute,
		Checks: []*ScalingCheck{
			{Name: "cpu", Metric: "nomad.cpu", Strategy: "target-value", StrategyConfig: map[string]string{"target": "70"}},
			{Name: "gone", Query: "q", Strategy: "threshold"},
		},
	}
	new := old.Copy()
	new.EvaluationInterval = 2 * time.Minute
	new.OnCheckError = ScalingOnErrorIgnore
	new.Checks[0].StrategyConfig["target"] = "80"
	new.Checks = append(new.Checks[:1], &ScalingCheck{Name: "added", Query: "q", Strategy: "threshold"})

	diff := scalingDiff(old, new, false)
	must.NotNil(t, diff)
	must.Eq(t, DiffTypeEdited, diff.Type)

	fields := map[string]*FieldDiff{}
	for _, f := range diff.Fields {
		fields[f.Name] = f
	}
	must.Eq(t, DiffTypeEdited, fields["EvaluationInterval"].Type)
	must.Eq(t, DiffTypeAdded, fields["OnCheckError"].Type)
	must.MapNotContainsKey(t, fields, "Cooldown")

	types := map[string]DiffType{}
	for _, o := range diff.Objects {
		if o.Name != "Check" {
			continue
		}
		for _, f := range o.Fields {
			if f.Name == "Name" {
				name := f.New
				if name == "" {
					name = f.Old
				}
				types[name] = o.Type
			}
		}
	}
	must.Eq(t, map[string]DiffType{
		"added": DiffTypeAdded,
		"cpu":   DiffTypeEdited,
		"gone":  DiffTypeDeleted,
	}, types)

	// A policy without typed fields keeps the diff of the document only.
	plain := &ScalingPolicy{Min: 1, Max: 5}
	plain2 := &ScalingPolicy{Min: 1, Max: 6}
	diff = scalingDiff(plain, plain2, false)
	must.Len(t, 1, diff.Fields)
	must.Eq(t, "Max", diff.Fields[0].Name)
}
