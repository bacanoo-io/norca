// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/hashicorp/nomad/nomad/structs"
)

// jobScalingHook handles the typed scaling policies of the built-in
// autoscaler at submission.
//
// The mutator fills the typed policy of every task group scaling policy from
// its untyped policy document when no typed field is set, so existing jobs
// and jobspecs keep working unchanged. The validator (jobScalingValidate in
// the design) rejects invalid policies of the builtin engine, so a faulty
// policy is reported by job plan and job run instead of the autoscaler's log.
type jobScalingHook struct {
	srv *Server
}

func (jobScalingHook) Name() string {
	return "scaling"
}

// Mutate fills the typed scaling policies. Errors in the policy documents
// are reported by Validate.
func (jobScalingHook) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	for _, tg := range job.TaskGroups {
		if tg.Scaling != nil {
			_ = tg.Scaling.FillTypedPolicy()
		}
	}
	return job, nil, nil
}

// Validate validates the scaling policies of the builtin engine.
func (h jobScalingHook) Validate(job *structs.Job) ([]error, error) {
	cfg := h.srv.autoscalerConfig()
	var mErr *multierror.Error

	for _, tg := range job.TaskGroups {
		p := tg.Scaling
		if p == nil || autoscaler.PolicyEngine(p, cfg.DefaultEngine) != structs.ScalingPolicyEngineBuiltin {
			continue
		}
		if p.Enabled && !cfg.Enabled {
			mErr = multierror.Append(mErr, fmt.Errorf(
				"task group %q: scaling policy uses the builtin engine, but the built-in autoscaler is not enabled on the servers",
				tg.Name))
		}
		if err := autoscaler.ValidatePolicy(p, cfg); err != nil {
			if me, ok := err.(*multierror.Error); ok {
				for _, e := range me.Errors {
					mErr = multierror.Append(mErr, fmt.Errorf("task group %q: scaling policy invalid: %w", tg.Name, e))
				}
			} else {
				mErr = multierror.Append(mErr, fmt.Errorf("task group %q: scaling policy invalid: %w", tg.Name, err))
			}
		}

		for _, task := range tg.Tasks {
			for _, tp := range task.ScalingPolicies {
				if tp != nil && tp.Engine == structs.ScalingPolicyEngineBuiltin {
					mErr = multierror.Append(mErr, fmt.Errorf(
						"task %q: task scaling policies are not supported by the builtin engine", task.Name))
				}
			}
		}
	}

	return nil, mErr.ErrorOrNil()
}

// autoscalerConfig returns the current configuration of the built-in
// autoscaler.
func (s *Server) autoscalerConfig() *autoscaler.Config {
	if s.autoscaler != nil {
		return s.autoscaler.Config()
	}
	if s.config.Autoscaler != nil {
		return s.config.Autoscaler.Copy()
	}
	return autoscaler.DefaultConfig()
}

// scaleBlockedByDeployment reports whether an active deployment of the job
// blocks scaling the group from prev to count. Scaling is blocked during an
// active deployment, except scaling up when the group's policy allows it
// with scale_during_deployment = "up_only" and no canary waits for
// promotion.
func scaleBlockedByDeployment(deployment *structs.Deployment, job *structs.Job,
	group *structs.TaskGroup, prev, count int64) bool {

	if deployment == nil || !deployment.Active() || deployment.JobCreateIndex != job.CreateIndex {
		return false
	}
	if group.Scaling == nil || count <= prev {
		return true
	}
	// Policies stored before the typed policy existed only have the
	// untyped document.
	if p, _ := autoscaler.TypedPolicy(group.Scaling); p.ScaleDuringDeployment != structs.ScaleDuringDeploymentUpOnly {
		return true
	}
	for _, ds := range deployment.TaskGroups {
		if ds != nil && ds.DesiredCanaries > 0 && !ds.Promoted {
			return true
		}
	}
	return false
}
