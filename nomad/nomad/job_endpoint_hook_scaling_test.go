// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
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
