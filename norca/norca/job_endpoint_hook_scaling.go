// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	multierror "github.com/hashicorp/go-multierror"
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

// keepScaledCounts keeps the task group counts of the stored job in a new
// version of it, before the new version is registered or planned.
//
// The count of a group that an enabled scaling policy scales, in the stored
// job and in the new one, belongs to the policy: the count of the job spec
// was right when the job was written and is not what the load needs now.
// Such a group keeps its count, brought within the bounds of the new policy,
// unless reset says to take the job spec's. Without a policy in both, or
// with a disabled one, the job spec's count is taken, as it always was.
//
// preserveAll keeps the count of every group that exists in both versions,
// with or without a policy (PreserveCounts).
func keepScaledCounts(existing, updated *structs.Job, preserveAll, reset bool) {
	if existing == nil || updated == nil {
		return
	}
	for _, tg := range updated.TaskGroups {
		old := existing.LookupTaskGroup(tg.Name)
		if old == nil {
			continue
		}
		switch {
		case preserveAll:
			tg.Count = old.Count
		case !reset && isScaledGroup(old) && isScaledGroup(tg):
			count := min(max(int64(old.Count), tg.Scaling.Min), tg.Scaling.Max)
			tg.Count = int(count)
		}
	}
}

// isScaledGroup reports whether an enabled scaling policy scales the count
// of the task group, whichever engine evaluates it.
func isScaledGroup(tg *structs.TaskGroup) bool {
	p := tg.Scaling
	return p != nil && p.Enabled && (p.Type == "" || p.Type == structs.ScalingPolicyTypeHorizontal)
}

// scaledByPolicyWarning is the warning of a count changed by someone else
// than the built-in autoscaler on a group that an enabled policy of the
// built-in engine scales. The change is made: it may be what an emergency
// needs. But the policy decides the count again once its cooldown, which the
// change starts, is over.
const scaledByPolicyWarning = "the count of this task group is decided by an enabled scaling policy of the " +
	"built-in autoscaler: the policy will set the count again after its cooldown. Disable the policy " +
	"or change its bounds to keep a count"

// handMadeScalingWarning returns scaledByPolicyWarning when a request to
// change the count of the group is not the built-in autoscaler's own and the
// group is scaled by an enabled policy of the built-in engine; "" otherwise.
func handMadeScalingWarning(group *structs.TaskGroup, args *structs.JobScaleRequest, defaultEngine string) string {
	if args.Count == nil || !isScaledGroup(group) || autoscaler.IsOwnScaling(args.Meta) {
		return ""
	}
	if autoscaler.PolicyEngine(group.Scaling, defaultEngine) != structs.ScalingPolicyEngineBuiltin {
		return ""
	}
	return scaledByPolicyWarning
}
