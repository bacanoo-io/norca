// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// fixedValuePolicy is a scaling policy document with a single fixed-value
// check, which needs no metrics.
func fixedValuePolicy(value int) map[string]interface{} {
	return map[string]interface{}{
		"evaluation_interval": "100ms",
		"cooldown":            "1h",
		"check": []interface{}{
			map[string]interface{}{
				"fixed": []interface{}{
					map[string]interface{}{
						"strategy": []interface{}{
							map[string]interface{}{
								"fixed-value": []interface{}{
									map[string]interface{}{"value": value},
								},
							},
						},
					},
				},
			},
		},
	}
}

func testAutoscalerConfig(c *Config) {
	// No schedulers: the job is never placed, so no deployment blocks
	// Job.Scale.
	c.NumSchedulers = 0
	c.Autoscaler = autoscaler.DefaultConfig()
	c.Autoscaler.Enabled = true
	c.Autoscaler.DefaultEngine = structs.ScalingPolicyEngineBuiltin
	c.Autoscaler.MinEvalInterval = 50 * time.Millisecond
}

// TestAutoscaler_ScalesThroughJobScale runs the built-in autoscaler on a test
// server and checks that it scales a job through the real Job.Scale path,
// with ACLs enabled, using its scale-job only system identity.
func TestAutoscaler_ScalesThroughJobScale(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanupS1 := TestACLServer(t, testAutoscalerConfig)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	job, _ := mock.JobWithScalingPolicy()
	job.TaskGroups[0].Count = 1
	job.TaskGroups[0].Scaling.Min = 1
	job.TaskGroups[0].Scaling.Max = 5
	job.TaskGroups[0].Scaling.Policy = fixedValuePolicy(3)

	req := &structs.JobRegisterRequest{
		Job: job,
		WriteRequest: structs.WriteRequest{
			Region:    "global",
			Namespace: job.Namespace,
			AuthToken: root.SecretID,
		},
	}
	var resp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))

	state := s1.fsm.State()
	testutil.WaitForResult(func() (bool, error) {
		out, err := state.JobByID(nil, job.Namespace, job.ID)
		if err != nil {
			return false, err
		}
		if c := out.TaskGroups[0].Count; c != 3 {
			return false, fmt.Errorf("expected count 3, got %d", c)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })

	// The decision went through Job.Scale: a scaling event with the
	// autoscaler's meta and an evaluation.
	events, _, err := state.ScalingEventsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	tgEvents := events[job.TaskGroups[0].Name]
	must.Len(t, 1, tgEvents)
	e := tgEvents[0]
	must.Eq(t, int64(1), e.PreviousCount)
	must.Eq(t, int64(3), *e.Count)
	must.NotNil(t, e.EvalID)
	must.Eq[any](t, structs.ScalingPolicyEngineBuiltin, e.Meta["nomad_autoscaler.engine"])

	eval, err := state.EvalByID(nil, *e.EvalID)
	must.NoError(t, err)
	must.Eq(t, structs.EvalTriggerScaling, eval.TriggeredBy)

	// The cooldown (1h) prevents further scaling, even after the policy
	// changes.
	job2 := job.Copy()
	job2.TaskGroups[0].Count = 3
	job2.TaskGroups[0].Scaling.Policy = fixedValuePolicy(4)
	req.Job = job2
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
	time.Sleep(500 * time.Millisecond)
	events, _, err = state.ScalingEventsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 1, events[job.TaskGroups[0].Name])
}

// TestAutoscaler_IgnoresExternalPolicies checks that policies of the external
// engine are left alone.
func TestAutoscaler_IgnoresExternalPolicies(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, testAutoscalerConfig)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	external, _ := mock.JobWithScalingPolicy()
	external.TaskGroups[0].Count = 1
	external.TaskGroups[0].Scaling.Min = 1
	external.TaskGroups[0].Scaling.Max = 5
	external.TaskGroups[0].Scaling.Engine = structs.ScalingPolicyEngineExternal
	external.TaskGroups[0].Scaling.Policy = fixedValuePolicy(3)

	builtin := external.Copy()
	builtin.ID = "builtin-" + external.ID
	builtin.TaskGroups[0].Scaling.ID = ""
	builtin.TaskGroups[0].Scaling.Engine = ""
	builtin.TaskGroups[0].Scaling.TargetTaskGroup(builtin, builtin.TaskGroups[0])

	for _, job := range []*structs.Job{external, builtin} {
		req := &structs.JobRegisterRequest{
			Job: job,
			WriteRequest: structs.WriteRequest{
				Region:    "global",
				Namespace: job.Namespace,
			},
		}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
	}

	state := s1.fsm.State()
	testutil.WaitForResult(func() (bool, error) {
		out, err := state.JobByID(nil, builtin.Namespace, builtin.ID)
		if err != nil {
			return false, err
		}
		if c := out.TaskGroups[0].Count; c != 3 {
			return false, fmt.Errorf("expected count 3, got %d", c)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })

	out, err := state.JobByID(nil, external.Namespace, external.ID)
	must.NoError(t, err)
	must.Eq(t, 1, out.TaskGroups[0].Count)
}

func TestAutoscaler_SystemIdentity(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanupS1 := TestACLServer(t, testAutoscalerConfig)
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	// The identity is valid on the leader only and resolves to an ACL that
	// can scale jobs and nothing else.
	secret := s1.getAutoscalerSecret()
	must.NotEq(t, "", secret)

	args := &structs.JobScaleRequest{WriteRequest: structs.WriteRequest{AuthToken: secret}}
	must.NoError(t, s1.Authenticate(nil, args))
	aclObj, err := s1.ResolveACL(args)
	must.NoError(t, err)
	must.True(t, aclObj.AllowNsOp("default", acl.NamespaceCapabilityScaleJob))
	must.True(t, aclObj.AllowNsOp("other", acl.NamespaceCapabilityScaleJob))
	must.False(t, aclObj.AllowNsOp("default", acl.NamespaceCapabilitySubmitJob))
	must.False(t, aclObj.AllowNsOp("default", acl.NamespaceCapabilityReadJob))
	must.False(t, aclObj.IsManagement())

	// Losing leadership invalidates the identity.
	must.NoError(t, s1.revokeLeadership())
	must.Eq(t, "", s1.getAutoscalerSecret())
	must.False(t, s1.autoscaler.Running())
	args = &structs.JobScaleRequest{WriteRequest: structs.WriteRequest{AuthToken: secret}}
	must.ErrorIs(t, s1.Authenticate(nil, args), structs.ErrPermissionDenied)
}

func TestScalingEndpoint_ListPolicies_EngineFilter(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	codec := rpcClient(t, s1)
	testutil.WaitForLeader(t, s1.RPC)

	pBuiltin := mock.ScalingPolicy()
	pBuiltin.Engine = structs.ScalingPolicyEngineBuiltin
	pExternal := mock.ScalingPolicy()
	pExternal.Engine = structs.ScalingPolicyEngineExternal
	pDefault := mock.ScalingPolicy()
	must.NoError(t, s1.fsm.State().UpsertScalingPolicies(1000,
		[]*structs.ScalingPolicy{pBuiltin, pExternal, pDefault}))

	list := func(engine, namespace string) ([]string, error) {
		req := &structs.ScalingPolicyListRequest{
			Engine: engine,
			QueryOptions: structs.QueryOptions{
				Region:    "global",
				Namespace: namespace,
			},
		}
		var resp structs.ScalingPolicyListResponse
		if err := msgpackrpc.CallWithCodec(codec, "Scaling.ListPolicies", req, &resp); err != nil {
			return nil, err
		}
		var ids []string
		for _, p := range resp.Policies {
			ids = append(ids, p.ID)
		}
		return ids, nil
	}

	for _, ns := range []string{structs.DefaultNamespace, structs.AllNamespacesSentinel} {
		ids, err := list("", ns)
		must.NoError(t, err)
		must.SliceContainsAll(t, []string{pBuiltin.ID, pExternal.ID, pDefault.ID}, ids)

		// The default engine is "external".
		ids, err = list(structs.ScalingPolicyEngineExternal, ns)
		must.NoError(t, err)
		must.SliceContainsAll(t, []string{pExternal.ID, pDefault.ID}, ids)

		ids, err = list(structs.ScalingPolicyEngineBuiltin, ns)
		must.NoError(t, err)
		must.Eq(t, []string{pBuiltin.ID}, ids)

		_, err = list("other", ns)
		must.ErrorContains(t, err, "engine must be")
	}

	// With default_engine = "builtin", policies without an engine move.
	cfg := autoscaler.DefaultConfig()
	cfg.DefaultEngine = structs.ScalingPolicyEngineBuiltin
	s1.autoscaler.SetConfig(cfg)

	ids, err := list(structs.ScalingPolicyEngineBuiltin, structs.DefaultNamespace)
	must.NoError(t, err)
	must.SliceContainsAll(t, []string{pBuiltin.ID, pDefault.ID}, ids)
}
