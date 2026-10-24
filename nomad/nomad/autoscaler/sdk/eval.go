// Copyright IBM Corp. 2020, 2025
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 sdk/eval.go and sdk/eval_oss.go.
// Modified: merged, uses the Nomad uuid helper.

package sdk

import (
	"time"

	"github.com/hashicorp/nomad/helper/uuid"
)

// ScalingEvaluation forms an individual analysis undertaken by the autoscaler
// in order to determine the desired state of a target.
type ScalingEvaluation struct {
	ID               string
	Policy           *ScalingPolicy
	CheckEvaluations []*ScalingCheckEvaluation
	CreateTime       time.Time
}

// NewScalingEvaluation creates a new ScalingEvaluation based off the passed
// policy and status. It is responsible for hydrating all the fields to a basic
// level for safe usage throughout the scaling evaluation phase.
func NewScalingEvaluation(p *ScalingPolicy) *ScalingEvaluation {

	// Create the base eval.
	eval := ScalingEvaluation{
		ID:         uuid.Generate(),
		Policy:     p,
		CreateTime: time.Now().UTC(),
	}

	// Iterate the policy checks and add then to the eval.
	for _, check := range p.Checks {
		checkEval := ScalingCheckEvaluation{
			Check: check,
			Action: &ScalingAction{
				Meta: map[string]interface{}{
					"nomad_policy_id": p.ID,
				},
			},
		}

		// Ensure the Action is canonicalized so we don't need to perform this
		// again.
		checkEval.Action.Canonicalize()

		// Append the check.
		eval.CheckEvaluations = append(eval.CheckEvaluations, &checkEval)
	}

	return &eval
}

// ScalingCheckEvaluation is the evaluation of an individual policy check. Each
// check eval within a ScalingEvaluation is performed concurrently and a single
// "winner" picked once all have returned.
type ScalingCheckEvaluation struct {

	// Check is the individual ScalingPolicyCheck that this eval is concerned
	// with.
	Check *ScalingPolicyCheck

	// Metrics is the metric resulting from querying the APM.
	Metrics TimestampedMetrics

	// Action is the calculated desired state and is populated by strategy.Run.
	Action *ScalingAction
}
