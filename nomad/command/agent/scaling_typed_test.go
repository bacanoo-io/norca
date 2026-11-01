// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/nomad/api"
	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/helper/pointer"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestApiScalingPolicyToStructs_Typed(t *testing.T) {
	ci.Parallel(t)

	ap := &api.ScalingPolicy{
		Max:                   pointer.Of(int64(10)),
		Engine:                "builtin",
		EvaluationInterval:    time.Minute,
		Cooldown:              2 * time.Minute,
		CooldownOnScaleUp:     time.Minute,
		OnCheckError:          "fail",
		ScaleDuringDeployment: api.ScaleDuringDeploymentUpOnly,
		Checks: []*api.ScalingCheck{{
			Name:           "cpu",
			Metric:         "nomad.cpu",
			QueryWindow:    time.Minute,
			Strategy:       "target-value",
			StrategyConfig: map[string]string{"target": "70"},
		}},
	}
	p := ApiScalingPolicyToStructs(2, ap)
	must.Eq(t, time.Minute, p.EvaluationInterval)
	must.Eq(t, 2*time.Minute, p.Cooldown)
	must.Eq(t, time.Minute, p.CooldownOnScaleUp)
	must.Eq(t, structs.ScalingOnErrorFail, p.OnCheckError)
	must.Eq(t, structs.ScaleDuringDeploymentUpOnly, p.ScaleDuringDeployment)
	must.Eq(t, []*structs.ScalingCheck{{
		Name:           "cpu",
		Metric:         "nomad.cpu",
		QueryWindow:    time.Minute,
		Strategy:       "target-value",
		StrategyConfig: map[string]string{"target": "70"},
	}}, p.Checks)

	// The API map is not shared with the server object.
	ap.Checks[0].StrategyConfig["target"] = "80"
	must.Eq(t, "70", p.Checks[0].StrategyConfig["target"])
}
