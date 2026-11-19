// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// A periodic parameterized job "cron": a dispatch makes a periodic child,
// whose launches are the jobs that run. A launch gets the ACL policies bound
// to "cron", as the dispatched children of a parameterized job and the
// launches of a periodic job do (review finding MUK-24). It used to look
// for policies bound to the dispatched child, its parent, which nobody
// binds, and got none; a change of the launch then needed none either.
func TestJobEndpoint_BoundPolicies_LaunchOfDispatchedPeriodic(t *testing.T) {
	ci.Parallel(t)
	f, cleanup := newBoundPolicyFixture(t)
	defer cleanup()
	store := f.srv.fsm.State()

	cron := mock.PeriodicJob()
	cron.ID, cron.Name = "cron", "cron"
	cron.ParameterizedJob = &structs.ParameterizedJobConfig{}

	dispatched := cron.Copy()
	dispatched.ID = "cron" + structs.DispatchLaunchSuffix + "1700000000-0123abcd"
	dispatched.Name = dispatched.ID
	dispatched.ParentID = cron.ID
	dispatched.Dispatched = true

	launch := dispatched.Copy()
	launch.ID = dispatched.ID + structs.PeriodicLaunchSuffix + "1700000060"
	launch.Name = launch.ID
	launch.ParentID = dispatched.ID
	launch.Periodic = nil

	index, err := store.LatestIndex()
	must.NoError(t, err)
	for i, job := range []*structs.Job{cron, dispatched, launch} {
		must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, index+1+uint64(i), nil, job.Copy()))
	}

	snap, err := store.Snapshot()
	must.NoError(t, err)
	for _, job := range []*structs.Job{cron, dispatched, launch} {
		id, err := boundPolicyJobID(snap, job)
		must.NoError(t, err)
		must.Eq(t, "cron", id, must.Sprint(job.ID))
	}

	// The tasks of a launch get the policy bound to "cron".
	alloc := mock.Alloc()
	alloc.Job = launch.Copy()
	alloc.JobID = launch.ID
	alloc.TaskGroup = launch.TaskGroups[0].Name
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index+10, []*structs.Allocation{alloc}))
	policies, err := f.srv.resolvePoliciesForClaims(alloc.ToTaskIdentityClaims(alloc.Job, launch.TaskGroups[0].Tasks[0].Name))
	must.NoError(t, err)
	var names []string
	for _, p := range policies {
		names = append(names, p.Name)
	}
	must.Eq(t, []string{"cron-policy"}, names)

	// Changing the launch needs the policy, and the error names "cron".
	changed := launch.Copy()
	changed.Meta = map[string]string{"injected": "yes"}
	err = f.register(changed, f.team.SecretID)
	must.ErrorContains(t, err, "403,Permission denied")
	must.ErrorContains(t, err, `the job "cron" its parent comes from`)
	must.NoError(t, f.register(changed, f.root.SecretID))

	// A parent that is gone ends the chain: the launch gets nothing.
	must.NoError(t, store.DeleteJob(index+20, dispatched.Namespace, dispatched.ID))
	policies, err = f.srv.resolvePoliciesForClaims(alloc.ToTaskIdentityClaims(alloc.Job, launch.TaskGroups[0].Tasks[0].Name))
	must.NoError(t, err)
	must.SliceEmpty(t, policies)
}
