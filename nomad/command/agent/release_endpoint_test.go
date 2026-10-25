// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/shoenig/test/must"
)

// releaseServiceJob is a service job of the mock driver with a fast
// deployment. A non-empty startError makes its tasks fail.
func releaseServiceJob(id, startError string) *api.Job {
	job := MockRunnableJob()
	job.ID, job.Name = pointer.Of(id), pointer.Of(id)
	job.Namespace = nil
	job.Region = nil
	job.TaskGroups[0].Networks = nil
	job.TaskGroups[0].Tasks[0].Config = map[string]any{"run_for": "1h"}
	if startError != "" {
		job.TaskGroups[0].Tasks[0].Config["start_error"] = startError
	}
	job.TaskGroups[0].RestartPolicy = &api.RestartPolicy{Attempts: pointer.Of(0), Mode: pointer.Of("fail")}
	job.TaskGroups[0].Tasks[0].RestartPolicy = &api.RestartPolicy{Attempts: pointer.Of(0), Mode: pointer.Of("fail")}
	job.TaskGroups[0].ReschedulePolicy = &api.ReschedulePolicy{Attempts: pointer.Of(0), Unlimited: pointer.Of(false)}
	job.Update = &api.UpdateStrategy{
		MaxParallel:      pointer.Of(1),
		MinHealthyTime:   pointer.Of(200 * time.Millisecond),
		HealthyDeadline:  pointer.Of(10 * time.Second),
		ProgressDeadline: pointer.Of(time.Duration(0)),
		AutoRevert:       pointer.Of(false),
	}
	return job
}

// releaseHookJob is a batch hook job of the mock driver.
func releaseHookJob(id, hook string, exitCode int) *api.Job {
	job := MockRunnableJob()
	job.ID, job.Name = pointer.Of(id), pointer.Of(id)
	job.Namespace = nil
	job.Region = nil
	job.Type = pointer.Of("batch")
	job.Update = nil
	job.TaskGroups[0].Update = nil
	job.TaskGroups[0].Migrate = nil
	job.TaskGroups[0].Networks = nil
	job.TaskGroups[0].Tasks[0].Config = map[string]any{"run_for": "100ms", "exit_code": exitCode}
	job.TaskGroups[0].RestartPolicy = &api.RestartPolicy{Attempts: pointer.Of(0), Mode: pointer.Of("fail")}
	job.TaskGroups[0].Tasks[0].RestartPolicy = &api.RestartPolicy{Attempts: pointer.Of(0), Mode: pointer.Of("fail")}
	job.TaskGroups[0].ReschedulePolicy = &api.ReschedulePolicy{Attempts: pointer.Of(0), Unlimited: pointer.Of(false)}
	job.Meta = map[string]string{api.ReleaseMetaHook: hook}
	return job
}

func TestHTTP_Release_AtomicDeploymentFailure(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()
		releases := client.Releases()

		ok, err := releases.Supported(nil)
		must.NoError(t, err)
		must.True(t, ok)

		// Revision 1 deploys successfully.
		resp, _, err := releases.Apply("svc", &api.ReleaseApplyRequest{
			Pack: &api.ReleasePack{Name: "svc", Version: "1.0.0"},
			Resources: []*api.ReleaseResource{
				{Type: api.ReleaseResourceJob, Job: releaseServiceJob("svc", "")},
				{Type: api.ReleaseResourceVariable, Variable: &api.Variable{Path: "config/svc", Items: map[string]string{"level": "info"}}},
			},
			Wait:    true,
			Timeout: 2 * time.Minute,
		}, nil)
		must.NoError(t, err)
		must.Eq(t, uint64(1), resp.Revision)
		must.Eq(t, api.ReleasePhaseDeployed, resp.Release.Status.Phase)
		must.Eq(t, "True", resp.Release.Status.Condition(api.ReleaseConditionReady).Status)

		// Revision 2 changes the variable and breaks the job: the deployment
		// fails and atomic mode restores revision 1.
		_, _, err = releases.Apply("svc", &api.ReleaseApplyRequest{
			Pack: &api.ReleasePack{Name: "svc", Version: "2.0.0"},
			Resources: []*api.ReleaseResource{
				{Type: api.ReleaseResourceJob, Job: releaseServiceJob("svc", "broken")},
				{Type: api.ReleaseResourceVariable, Variable: &api.Variable{Path: "config/svc", Items: map[string]string{"level": "debug"}}},
			},
			Atomic:  pointer.Of(true),
			Wait:    true,
			Timeout: 2 * time.Minute,
		}, nil)
		must.ErrorContains(t, err, "rolled back to revision 1")

		rel, _, err := releases.Info("svc", nil)
		must.NoError(t, err)
		must.Eq(t, api.ReleasePhaseFailed, rel.Status.Phase)
		must.Eq(t, uint64(1), rel.Status.Revision)
		must.True(t, rel.Status.LastOperation.RolledBack)

		job, _, err := client.Jobs().Info("svc", nil)
		must.NoError(t, err)
		_, broken := job.TaskGroups[0].Tasks[0].Config["start_error"]
		must.False(t, broken)
		must.Eq(t, uint64(2), *job.Version) // restored as a new version
		v, _, err := client.Variables().Read("config/svc", nil)
		must.NoError(t, err)
		must.Eq(t, "info", v.Items["level"])

		hist, _, err := releases.History("svc", nil)
		must.NoError(t, err)
		must.Len(t, 2, hist)
		must.Eq(t, api.ReleasePhaseFailed, hist[0].Status.Phase)
		must.StrContains(t, hist[0].Status.Message, "deployment")
		must.Eq(t, api.ReleasePhaseDeployed, hist[1].Status.Phase)

		// A drift check finds nothing: the job was restored.
		drift, _, err := releases.Drift("svc", nil)
		must.NoError(t, err)
		must.False(t, drift.Drifted, must.Sprintf("%+v", drift.Resources))

		_, _, err = releases.Delete("svc", nil, nil)
		must.NoError(t, err)
		_, _, err = client.Jobs().Info("svc", nil)
		must.ErrorContains(t, err, "404")
	})
}

func TestHTTP_Release_HooksAndTests(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()
		releases := client.Releases()
		suffix := uuid.Short()

		pre := releaseHookJob("pre-"+suffix, "pre-install", 0)
		keep := releaseHookJob("keep-"+suffix, "post-install", 0)
		keep.Meta[api.ReleaseMetaHookDelete] = "never"
		test := releaseHookJob("test-"+suffix, "test", 0)
		resp, _, err := releases.Apply("hooked", &api.ReleaseApplyRequest{
			Pack: &api.ReleasePack{Name: "hooked"},
			Resources: []*api.ReleaseResource{
				{Type: api.ReleaseResourceJob, Job: pre},
				{Type: api.ReleaseResourceJob, Job: keep},
				{Type: api.ReleaseResourceJob, Job: test},
			},
			Timeout: 2 * time.Minute,
		}, nil)
		must.NoError(t, err)
		must.Eq(t, "True", resp.Release.Status.Condition(api.ReleaseConditionHooksSucceeded).Status)
		must.Len(t, 0, resp.Release.Status.Resources)

		// The pre-install hook was purged, the kept one stays; the test hook
		// did not run.
		_, _, err = client.Jobs().Info(*pre.ID, nil)
		must.ErrorContains(t, err, "404")
		_, _, err = client.Jobs().Info(*keep.ID, nil)
		must.NoError(t, err)
		_, _, err = client.Jobs().Info(*test.ID, nil)
		must.ErrorContains(t, err, "404")

		result, _, err := releases.Test("hooked", time.Minute, nil)
		must.NoError(t, err)
		must.True(t, result.Succeeded)
		must.Len(t, 1, result.Hooks)
		must.Eq(t, "succeeded", result.Hooks[0].Status)
		rel, _, err := releases.Info("hooked", nil)
		must.NoError(t, err)
		must.True(t, rel.Status.LastTest.Succeeded)

		// A failing post-upgrade hook fails the upgrade.
		failing := releaseHookJob("fail-"+suffix, "post-upgrade", 1)
		_, _, err = releases.Apply("hooked", &api.ReleaseApplyRequest{
			Pack: &api.ReleasePack{Name: "hooked"},
			Resources: []*api.ReleaseResource{
				{Type: api.ReleaseResourceJob, Job: failing},
			},
			Atomic:  pointer.Of(false),
			Timeout: 2 * time.Minute,
		}, nil)
		must.ErrorContains(t, err, "post-upgrade hook job")
		rel, _, err = releases.Info("hooked", nil)
		must.NoError(t, err)
		must.Eq(t, "False", rel.Status.Condition(api.ReleaseConditionHooksSucceeded).Status)
		// The failed hook job is kept for debugging (on-success policy).
		_, _, err = client.Jobs().Info(*failing.ID, nil)
		must.NoError(t, err)

		// Uninstall removes the kept hook jobs too.
		_, _, err = releases.Delete("hooked", nil, nil)
		must.NoError(t, err)
		_, _, err = client.Jobs().Info(*keep.ID, nil)
		must.ErrorContains(t, err, "404")
	})
}

func TestHTTP_Release_PlanAndRevision(t *testing.T) {
	ci.Parallel(t)
	httpTest(t, nil, func(s *TestAgent) {
		releases := s.Client().Releases()
		req := &api.ReleaseApplyRequest{
			Pack: &api.ReleasePack{Name: "plan"},
			Resources: []*api.ReleaseResource{
				{Type: api.ReleaseResourceNamespace, Namespace: &api.Namespace{Name: "planned", Description: "a"}},
				{Type: api.ReleaseResourceJob, Job: releaseServiceJob("planned", "")},
			},
		}
		plan, _, err := releases.Plan("plan", req, nil)
		must.NoError(t, err)
		must.Len(t, 2, plan.Changes)
		must.Eq(t, "namespace", plan.Changes[0].Type)
		must.Eq(t, "create", plan.Changes[0].Action)
		must.Eq(t, "create", plan.Changes[1].Action)
		must.NotNil(t, plan.Changes[1].JobDiff)

		// Nothing was written.
		_, _, err = releases.Info("plan", nil)
		must.ErrorContains(t, err, "404")

		_, _, err = releases.Apply("plan", req, nil)
		must.NoError(t, err)
		rev, _, err := releases.Revision("plan", 1, nil)
		must.NoError(t, err)
		must.Len(t, 2, rev.Resources)
		must.Eq(t, "planned", rev.Resources[0].Namespace.Name)
		must.Eq(t, "planned", *rev.Resources[1].Job.ID)
		must.Eq(t, "plan", rev.Resources[1].Job.Meta[api.ReleaseMetaRelease])

		list, _, err := releases.List(nil)
		must.NoError(t, err)
		must.Len(t, 1, list)
	})
}
