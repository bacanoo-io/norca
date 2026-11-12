// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// typedPolicyDocument is a policy document in the typed check form, as the
// HCL parsers produce it.
func typedPolicyDocument(check map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"evaluation_interval":     "30s",
		"cooldown":                "2m",
		"scale_during_deployment": "up_only",
		"check": []interface{}{
			map[string]interface{}{"cpu": []interface{}{check}},
		},
	}
}

func scalingJob(engine string, doc map[string]interface{}) *structs.Job {
	job, _ := mock.JobWithScalingPolicy()
	job.TaskGroups[0].Count = 2
	job.TaskGroups[0].Scaling.Min = 1
	job.TaskGroups[0].Scaling.Max = 10
	job.TaskGroups[0].Scaling.Engine = engine
	job.TaskGroups[0].Scaling.Policy = doc
	return job
}

func TestJobEndpoint_Plan_InvalidBuiltinScalingPolicy(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, testAutoscalerConfig)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	cases := []struct {
		name  string
		check map[string]interface{}
		err   string
	}{
		{
			name:  "missing strategy parameter",
			check: map[string]interface{}{"metric": "nomad.cpu", "strategy": "target-value"},
			err:   "missing required field `target`",
		},
		{
			name:  "unknown strategy",
			check: map[string]interface{}{"metric": "nomad.cpu", "strategy": "magic", "target": 70},
			err:   `unknown strategy "magic"`,
		},
		{
			name:  "unknown source",
			check: map[string]interface{}{"source": "datadog", "query": "q", "strategy": "target-value", "target": 70},
			err:   `unknown source "datadog"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := scalingJob(structs.ScalingPolicyEngineBuiltin, typedPolicyDocument(tc.check))
			req := &structs.JobPlanRequest{
				Job:  job,
				Diff: true,
				WriteRequest: structs.WriteRequest{
					Region:    "global",
					Namespace: job.Namespace,
				},
			}
			var resp structs.JobPlanResponse
			err := msgpackrpc.CallWithCodec(codec, "Job.Plan", req, &resp)
			must.ErrorContains(t, err, `task group "web": scaling policy invalid`)
			must.ErrorContains(t, err, tc.err)
		})
	}

	// The same policies are accepted for the external engine, as before.
	job := scalingJob(structs.ScalingPolicyEngineExternal, typedPolicyDocument(cases[0].check))
	req := &structs.JobPlanRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	var resp structs.JobPlanResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Plan", req, &resp))
}

func TestJobEndpoint_Register_TypedScalingPolicy(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, testAutoscalerConfig)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	job := scalingJob(structs.ScalingPolicyEngineBuiltin, typedPolicyDocument(map[string]interface{}{
		"metric":   "nomad.cpu",
		"strategy": "target-value",
		"target":   70,
	}))
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))

	// The typed policy is filled from the document and stored.
	out, err := s1.fsm.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	p := out.TaskGroups[0].Scaling
	must.Eq(t, 30*time.Second, p.EvaluationInterval)
	must.Eq(t, 2*time.Minute, p.Cooldown)
	must.Eq(t, structs.ScaleDuringDeploymentUpOnly, p.ScaleDuringDeployment)
	must.Eq(t, []*structs.ScalingCheck{{
		Name:           "cpu",
		Metric:         "nomad.cpu",
		Strategy:       "target-value",
		StrategyConfig: map[string]string{"target": "70"},
	}}, p.Checks)

	policy, err := s1.fsm.State().ScalingPolicyByID(nil, p.ID)
	must.NoError(t, err)
	must.Eq(t, p.Checks, policy.Checks)
}

func TestJobEndpoint_Register_BuiltinEngineDisabled(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	job := scalingJob(structs.ScalingPolicyEngineBuiltin, typedPolicyDocument(map[string]interface{}{
		"metric":   "nomad.cpu",
		"strategy": "target-value",
		"target":   70,
	}))
	req := &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}
	var resp structs.JobRegisterResponse
	err := msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp)
	must.ErrorContains(t, err, "built-in autoscaler is not enabled")
}

func TestScaleBlockedByDeployment(t *testing.T) {
	ci.Parallel(t)

	job := mock.Job()
	job.CreateIndex = 10
	tg := job.TaskGroups[0]
	tg.Scaling = &structs.ScalingPolicy{Min: 1, Max: 10}

	active := func(canary bool) *structs.Deployment {
		d := structs.NewDeployment(job, 50)
		d.JobCreateIndex = job.CreateIndex
		d.TaskGroups = map[string]*structs.DeploymentState{tg.Name: {DesiredTotal: 2}}
		if canary {
			d.TaskGroups[tg.Name].DesiredCanaries = 1
		}
		return d
	}

	// No or an inactive deployment never blocks.
	must.False(t, scaleBlockedByDeployment(nil, job, tg, 2, 3))
	done := active(false)
	done.Status = structs.DeploymentStatusSuccessful
	must.False(t, scaleBlockedByDeployment(done, job, tg, 2, 3))

	// By default an active deployment blocks scaling.
	must.True(t, scaleBlockedByDeployment(active(false), job, tg, 2, 3))

	// up_only allows scaling up, but not down and not with canaries.
	tg.Scaling.ScaleDuringDeployment = structs.ScaleDuringDeploymentUpOnly
	must.False(t, scaleBlockedByDeployment(active(false), job, tg, 2, 3))
	must.True(t, scaleBlockedByDeployment(active(false), job, tg, 2, 1))
	must.True(t, scaleBlockedByDeployment(active(true), job, tg, 2, 3))

	// Policies stored with only the document are understood too.
	tg.Scaling.ScaleDuringDeployment = ""
	tg.Scaling.Policy = map[string]interface{}{"scale_during_deployment": "up_only"}
	must.False(t, scaleBlockedByDeployment(active(false), job, tg, 2, 3))
}

func TestKeepScaledCounts(t *testing.T) {
	ci.Parallel(t)

	// group returns a task group with the count, and a scaling policy of
	// 1 to 10 if scaled.
	group := func(name string, count int, scaled bool) *structs.TaskGroup {
		tg := &structs.TaskGroup{Name: name, Count: count}
		if scaled {
			tg.Scaling = &structs.ScalingPolicy{
				Type: structs.ScalingPolicyTypeHorizontal, Min: 1, Max: 10, Enabled: true,
			}
		}
		return tg
	}
	with := func(tg *structs.TaskGroup, change func(*structs.ScalingPolicy)) *structs.TaskGroup {
		change(tg.Scaling)
		return tg
	}

	tests := []struct {
		name        string
		existing    *structs.TaskGroup
		updated     *structs.TaskGroup
		preserveAll bool
		reset       bool
		expected    int
	}{
		{
			name:     "a scaled group keeps its count",
			existing: group("web", 5, true),
			updated:  group("web", 2, true),
			expected: 5,
		},
		{
			name:     "reset takes the count of the job spec",
			existing: group("web", 5, true),
			updated:  group("web", 2, true),
			reset:    true,
			expected: 2,
		},
		{
			name:     "a group without a policy takes the count of the job spec",
			existing: group("web", 5, false),
			updated:  group("web", 2, false),
			expected: 2,
		},
		{
			name:     "a policy added by the new version does not own the old count",
			existing: group("web", 5, false),
			updated:  group("web", 2, true),
			expected: 2,
		},
		{
			name:     "a policy removed by the new version gives the count back",
			existing: group("web", 5, true),
			updated:  group("web", 2, false),
			expected: 2,
		},
		{
			name:     "a policy disabled by the new version gives the count back",
			existing: group("web", 5, true),
			updated:  with(group("web", 2, true), func(p *structs.ScalingPolicy) { p.Enabled = false }),
			expected: 2,
		},
		{
			name:     "a disabled policy did not scale the old count",
			existing: with(group("web", 5, true), func(p *structs.ScalingPolicy) { p.Enabled = false }),
			updated:  group("web", 2, true),
			expected: 2,
		},
		{
			name:     "the kept count is brought under the new maximum",
			existing: group("web", 8, true),
			updated:  with(group("web", 2, true), func(p *structs.ScalingPolicy) { p.Max = 6 }),
			expected: 6,
		},
		{
			name:     "the kept count is brought over the new minimum",
			existing: group("web", 2, true),
			updated:  with(group("web", 4, true), func(p *structs.ScalingPolicy) { p.Min = 3 }),
			expected: 3,
		},
		{
			name:     "a policy of the external engine keeps the count too",
			existing: with(group("web", 5, true), func(p *structs.ScalingPolicy) { p.Engine = structs.ScalingPolicyEngineExternal }),
			updated:  with(group("web", 2, true), func(p *structs.ScalingPolicy) { p.Engine = structs.ScalingPolicyEngineExternal }),
			expected: 5,
		},
		{
			name:        "preserve keeps the count of a group without a policy",
			existing:    group("web", 5, false),
			updated:     group("web", 2, false),
			preserveAll: true,
			expected:    5,
		},
		{
			name:        "preserve keeps a count outside the new bounds, as it did",
			existing:    group("web", 8, true),
			updated:     with(group("web", 2, true), func(p *structs.ScalingPolicy) { p.Max = 6 }),
			preserveAll: true,
			expected:    8,
		},
		{
			name:     "a group the stored job does not have takes the count of the job spec",
			existing: group("api", 5, true),
			updated:  group("web", 2, true),
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := &structs.Job{TaskGroups: []*structs.TaskGroup{tt.existing}}
			updated := &structs.Job{TaskGroups: []*structs.TaskGroup{tt.updated}}

			keepScaledCounts(existing, updated, tt.preserveAll, tt.reset)

			must.Eq(t, tt.expected, updated.TaskGroups[0].Count)
		})
	}

	t.Run("a new job has nothing to keep", func(t *testing.T) {
		updated := &structs.Job{TaskGroups: []*structs.TaskGroup{group("web", 2, true)}}
		keepScaledCounts(nil, updated, false, false)
		must.Eq(t, 2, updated.TaskGroups[0].Count)
	})
}

func TestHandMadeScalingWarning(t *testing.T) {
	ci.Parallel(t)

	scaled := func(engine string, enabled bool) *structs.TaskGroup {
		return &structs.TaskGroup{Name: "web", Count: 2, Scaling: &structs.ScalingPolicy{
			Type: structs.ScalingPolicyTypeHorizontal, Min: 1, Max: 10, Enabled: enabled, Engine: engine,
		}}
	}
	count := func(meta map[string]interface{}) *structs.JobScaleRequest {
		return &structs.JobScaleRequest{Count: pointer.Of(int64(5)), Meta: meta}
	}
	ownMeta := map[string]interface{}{"nomad_autoscaler.engine": structs.ScalingPolicyEngineBuiltin}

	tests := []struct {
		name          string
		group         *structs.TaskGroup
		args          *structs.JobScaleRequest
		defaultEngine string
		expected      string
	}{
		{
			name:     "a change by hand on a group of a built-in policy",
			group:    scaled(structs.ScalingPolicyEngineBuiltin, true),
			args:     count(nil),
			expected: scaledByPolicyWarning,
		},
		{
			name:          "the default engine decides for a policy without one",
			group:         scaled("", true),
			args:          count(nil),
			defaultEngine: structs.ScalingPolicyEngineBuiltin,
			expected:      scaledByPolicyWarning,
		},
		{
			name:  "the autoscaler's own change",
			group: scaled(structs.ScalingPolicyEngineBuiltin, true),
			args:  count(ownMeta),
		},
		{
			name:  "a policy of the external engine",
			group: scaled(structs.ScalingPolicyEngineExternal, true),
			args:  count(nil),
		},
		{
			name:          "a policy without an engine while the default is external",
			group:         scaled("", true),
			args:          count(nil),
			defaultEngine: structs.ScalingPolicyEngineExternal,
		},
		{
			name:  "a disabled policy",
			group: scaled(structs.ScalingPolicyEngineBuiltin, false),
			args:  count(nil),
		},
		{
			name:  "a group without a policy",
			group: &structs.TaskGroup{Name: "web", Count: 2},
			args:  count(nil),
		},
		{
			name:  "an event without a count changes nothing",
			group: scaled(structs.ScalingPolicyEngineBuiltin, true),
			args:  &structs.JobScaleRequest{Message: "note"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, handMadeScalingWarning(tt.group, tt.args, tt.defaultEngine))
		})
	}
}
