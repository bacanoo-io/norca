// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package scheduler

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testSchedNetworkPolicy(name, enforcement string) *structs.NetworkPolicy {
	return &structs.NetworkPolicy{
		Name:        name,
		Namespace:   structs.DefaultNamespace,
		Selector:    map[string]string{"app": "db"},
		Egress:      []*structs.NetworkPolicyRule{{Ports: []string{"443/tcp"}}},
		Enforcement: enforcement,
	}
}

func TestNetworkPolicyChecker(t *testing.T) {
	ci.Parallel(t)

	store, ctx := testContext(t)
	capable := mock.Node()
	capable.Attributes[structs.NetworkPolicyNftablesAttr] = "true"
	plain := mock.Node()

	job := mock.Job()
	job.Meta = map[string]string{"app": "db"}
	tg := job.TaskGroups[0]
	tg.Networks = structs.Networks{{Mode: "bridge"}}

	checker := NewNetworkPolicyChecker(ctx)
	checker.SetJob(job)

	// No policy: every node is feasible.
	checker.SetTaskGroup(tg)
	must.True(t, checker.Feasible(capable))
	must.True(t, checker.Feasible(plain))

	// Best effort policies do not filter.
	must.NoError(t, store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 100,
		[]*structs.NetworkPolicy{testSchedNetworkPolicy("best", structs.NetworkPolicyEnforcementBestEffort)}))
	checker.SetTaskGroup(tg)
	must.True(t, checker.Feasible(plain))

	// A required policy needs nftables on the node.
	must.NoError(t, store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 101,
		[]*structs.NetworkPolicy{testSchedNetworkPolicy("req", structs.NetworkPolicyEnforcementRequired)}))
	checker.SetTaskGroup(tg)
	must.True(t, checker.Feasible(capable))
	must.False(t, checker.Feasible(plain))

	// Host mode cannot enforce rules on any node.
	tg.Networks = nil
	checker.SetTaskGroup(tg)
	ctx.Reset()
	must.False(t, checker.Feasible(capable))
	must.MapContainsKey(t, ctx.Metrics().ConstraintFiltered, `network mode "host" cannot enforce network policies`)

	// Groups the policy does not select are not affected.
	job.Meta = map[string]string{"app": "web"}
	checker.SetJob(job)
	checker.SetTaskGroup(tg)
	must.True(t, checker.Feasible(plain))

	// Admin policies with required enforcement filter too.
	must.NoError(t, store.UpsertAdminNetworkPolicies(structs.MsgTypeTestSetup, 102,
		[]*structs.AdminNetworkPolicy{{Name: "adm", Enforcement: structs.NetworkPolicyEnforcementRequired}}))
	checker.SetTaskGroup(tg)
	must.False(t, checker.Feasible(capable))
}

func networkPolicyHarness(t *testing.T, policies ...*structs.NetworkPolicy) (*Harness, *structs.Job) {
	h := NewHarness(t)
	for i := 0; i < 3; i++ {
		node := mock.Node()
		node.Attributes[structs.NetworkPolicyNftablesAttr] = "true"
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}
	if len(policies) > 0 {
		must.NoError(t, h.State.UpsertNetworkPolicies(structs.MsgTypeTestSetup, h.NextIndex(), policies))
	}

	// mock.Job uses the host network mode.
	job := mock.Job()
	job.Meta = map[string]string{"app": "db"}
	job.TaskGroups[0].Count = 2
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	return h, job
}

func TestNetworkPolicy_RequiredHostModeNotPlaced(t *testing.T) {
	ci.Parallel(t)

	h, job := networkPolicyHarness(t, testSchedNetworkPolicy("req", structs.NetworkPolicyEnforcementRequired))
	must.NoError(t, h.Process(NewServiceScheduler, quotaRegisterEval(t, h, job)))

	must.Len(t, 0, h.Plans)
	must.Len(t, 1, h.Evals)
	metrics := h.Evals[0].FailedTGAllocs[job.TaskGroups[0].Name]
	must.NotNil(t, metrics)
	// Every node is filtered; the reason is recorded for the first node of
	// the class, later selections hit the class eligibility cache.
	must.Eq(t, 3, metrics.NodesFiltered)
}

func TestNetworkPolicy_BestEffortAndNoPolicyPlaced(t *testing.T) {
	ci.Parallel(t)

	for name, policies := range map[string][]*structs.NetworkPolicy{
		"no policy":   nil,
		"best effort": {testSchedNetworkPolicy("best", structs.NetworkPolicyEnforcementBestEffort)},
	} {
		t.Run(name, func(t *testing.T) {
			h, job := networkPolicyHarness(t, policies...)
			must.NoError(t, h.Process(NewServiceScheduler, quotaRegisterEval(t, h, job)))
			must.Len(t, 1, h.Plans)
			must.Len(t, 2, plannedAllocs(h.Plans[0]))
		})
	}
}

func TestNetworkPolicy_SystemSchedulerRequired(t *testing.T) {
	ci.Parallel(t)

	h, _ := networkPolicyHarness(t, testSchedNetworkPolicy("req", structs.NetworkPolicyEnforcementRequired))
	job := mock.SystemJob()
	job.Meta = map[string]string{"app": "db"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
	must.NoError(t, h.Process(NewSystemScheduler, quotaRegisterEval(t, h, job)))
	for _, plan := range h.Plans {
		must.Len(t, 0, plannedAllocs(plan))
	}
}
