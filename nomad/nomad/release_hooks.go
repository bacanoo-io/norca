// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Hooks are batch jobs of a pack marked with the pack.hook meta. They run
// in sequence by weight (pack.hook_weight, then job ID) in their phase, with
// the permissions of the caller; a phase succeeds when every hook job
// completed successfully.

// registeredJob is a job registered by an operation.
type registeredJob struct {
	namespace string
	id        string
	evalID    string
}

// hooksOf returns the hook jobs of a phase in execution order.
func hooksOf(resources []*structs.ReleaseResource, phase string) []*structs.ReleaseResource {
	var hooks []*structs.ReleaseResource
	for _, res := range resources {
		if res.HasHookPhase(phase) {
			hooks = append(hooks, res)
		}
	}
	sort.SliceStable(hooks, func(i, j int) bool {
		if wi, wj := hooks[i].HookWeight(), hooks[j].HookWeight(); wi != wj {
			return wi < wj
		}
		return hooks[i].Job.ID < hooks[j].Job.ID
	})
	return hooks
}

// runHooks runs the hooks of a phase and fails on the first hook that does
// not succeed.
func (op *releaseOp) runHooks(resources []*structs.ReleaseResource, phase string) error {
	for _, hook := range hooksOf(resources, phase) {
		result := op.runHook(hook, phase)
		op.hooks = append(op.hooks, result)
		op.changes = append(op.changes, &structs.ReleaseResourceChange{
			Type: structs.ReleaseResourceJob, Namespace: result.Namespace, Name: result.Job,
			Action: structs.ReleaseChangeHook, Hook: phase,
		})
		if result.Status != structs.ReleaseHookSucceeded {
			return fmt.Errorf("%s hook job %q %s: %s", phase, result.Job, result.Status, result.Message)
		}
	}
	return nil
}

// runHook runs a hook job to completion: an earlier run of the job is
// purged, the job is registered and waited for, and it is purged afterwards
// according to its deletion policy.
func (op *releaseOp) runHook(res *structs.ReleaseResource, phase string) *structs.ReleaseHookResult {
	job := res.Job
	result := &structs.ReleaseHookResult{Phase: phase, Namespace: job.Namespace, Job: job.ID}
	ref := &structs.ReleaseResourceRef{Type: structs.ReleaseResourceJob, Namespace: job.Namespace, Name: job.ID}

	fail := func(status, format string, args ...any) *structs.ReleaseHookResult {
		result.Status, result.Message = status, fmt.Sprintf(format, args...)
		if res.HookDeletePolicy() == structs.ReleaseHookDeleteAlways {
			_ = op.deleteResource(ref)
		}
		return result
	}

	if err := op.deleteResource(ref); err != nil {
		return fail(structs.ReleaseHookFailed, "failed to purge the previous run: %v", err)
	}
	if _, err := op.applyResource(res); err != nil {
		return fail(structs.ReleaseHookFailed, "failed to register: %v", err)
	}

	ctx, cancel := context.WithTimeout(op.ctx, res.HookTimeout())
	defer cancel()
	ok, msg, err := op.waitHookJob(ctx, job)
	switch {
	case err != nil:
		// Stop a hook that ran out of time, unless it is kept for debugging.
		if res.HookDeletePolicy() != structs.ReleaseHookDeleteNever {
			_ = op.deleteResource(ref)
		}
		return fail(structs.ReleaseHookTimeout, "%v", err)
	case !ok:
		return fail(structs.ReleaseHookFailed, "%s", msg)
	}

	result.Status, result.Message = structs.ReleaseHookSucceeded, msg
	if res.HookDeletePolicy() != structs.ReleaseHookDeleteNever {
		if err := op.deleteResource(ref); err != nil {
			op.warn("failed to purge hook job %q: %v", job.ID, err)
		}
	}
	return result
}

// waitHookJob waits until a hook job has finished: all its evaluations and
// allocations are terminal. It succeeds when every task group has as many
// completed allocations as its count (one per node for sysbatch jobs).
func (op *releaseOp) waitHookJob(ctx context.Context, hook *structs.Job) (bool, string, error) {
	store := op.m.srv.State()
	for {
		ws := memdb.NewWatchSet()
		job, err := store.JobByID(ws, hook.Namespace, hook.ID)
		if err != nil {
			return false, "", err
		}
		if job == nil {
			return false, "the hook job was removed", nil
		}
		evals, err := store.EvalsByJob(ws, hook.Namespace, hook.ID)
		if err != nil {
			return false, "", err
		}
		allocs, err := store.AllocsByJob(ws, hook.Namespace, hook.ID, false)
		if err != nil {
			return false, "", err
		}

		done := len(allocs) > 0 || job.Status == structs.JobStatusDead
		for _, e := range evals {
			if !e.TerminalStatus() {
				done = false
			}
		}
		complete := map[string]int{}
		failed := map[string]int{}
		for _, a := range allocs {
			switch {
			case !a.TerminalStatus():
				done = false
			case a.ClientStatus == structs.AllocClientStatusComplete:
				complete[a.TaskGroup]++
			default:
				failed[a.TaskGroup]++
			}
		}
		if done {
			ok, msg := true, ""
			for _, tg := range job.TaskGroups {
				want := tg.Count
				if job.Type == structs.JobTypeSysBatch {
					want = 1
				}
				if complete[tg.Name] < want {
					ok = false
					msg += fmt.Sprintf("group %q: %d of %d allocations completed, %d failed; ",
						tg.Name, complete[tg.Name], want, failed[tg.Name])
				}
			}
			if ok {
				return true, "completed", nil
			}
			return false, strings.TrimSuffix(msg, "; "), nil
		}
		if err := ws.WatchCtx(ctx); err != nil {
			return false, "", fmt.Errorf("the hook job did not finish in time (status %q)", job.Status)
		}
	}
}

// waitJobs waits until the deployments of the registered jobs succeed.
func (op *releaseOp) waitJobs(jobs []*registeredJob) error {
	for _, j := range jobs {
		if err := op.waitJob(j); err != nil {
			return err
		}
	}
	return nil
}

// waitJob waits for the deployment of the current version of a job. Jobs
// without a deployment (batch, system, unchanged) are done when their
// evaluation is.
func (op *releaseOp) waitJob(j *registeredJob) error {
	store := op.m.srv.State()
	for {
		ws := memdb.NewWatchSet()
		job, err := store.JobByID(ws, j.namespace, j.id)
		if err != nil {
			return err
		}
		if job == nil {
			return fmt.Errorf("job %q (namespace %q) disappeared", j.id, j.namespace)
		}
		if job.Stop || job.IsPeriodic() || job.IsParameterized() {
			return nil
		}

		evalDone := true
		if j.evalID != "" {
			eval, err := store.EvalByID(ws, j.evalID)
			if err != nil {
				return err
			}
			if eval != nil {
				if eval.Status == structs.EvalStatusFailed {
					return fmt.Errorf("evaluation %s of job %q failed: %s", eval.ID, j.id, eval.StatusDescription)
				}
				evalDone = eval.TerminalStatus()
			}
		}

		deployments, err := store.DeploymentsByJobID(ws, j.namespace, j.id, true)
		if err != nil {
			return err
		}
		var d *structs.Deployment
		for _, cand := range deployments {
			if cand.JobVersion == job.Version && cand.JobCreateIndex == job.CreateIndex {
				d = cand
				break
			}
		}
		if d != nil {
			switch d.Status {
			case structs.DeploymentStatusSuccessful:
				return nil
			case structs.DeploymentStatusFailed, structs.DeploymentStatusCancelled:
				return fmt.Errorf("deployment %s of job %q %s: %s", d.ID[:8], j.id, d.Status, d.StatusDescription)
			}
		} else if evalDone {
			return nil
		}

		if err := ws.WatchCtx(op.ctx); err != nil {
			return fmt.Errorf("timed out waiting for the deployment of job %q", j.id)
		}
	}
}
