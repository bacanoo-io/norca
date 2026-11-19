// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package deploymentwatcher

import (
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	mocker "github.com/stretchr/testify/mock"
)

// An auto revert registers an older version of the job through Raft, without
// the check that a registration changing a job with bound ACL policies
// needs a token holding them (JOG-2). It goes only to a stable version
// registered after the policies bound to the job last changed (review
// finding JOG-17). Before, a token with submit-job marked its own version
// from before the binding stable (Job.Stable) and failed the deployment of
// the policy holder (Deployment.Fail): the auto revert ran its version
// under the bound policies.
func TestWatcher_FailDeployment_RollbackBoundPolicies(t *testing.T) {
	ci.Parallel(t)

	// Each step makes a version of the job (stable or not) or binds a
	// policy to it; the last version is the one being deployed.
	type step struct {
		version  bool
		stable   bool
		bind     bool
		unstable []uint64 // versions marked unstable afterwards
		stableAt []uint64 // versions marked stable afterwards
	}
	cases := []struct {
		name     string
		steps    []step
		revertTo *uint64 // nil: no rollback
		desc     string
	}{
		{
			name:     "stable version before the binding",
			steps:    []step{{version: true, stable: true}, {bind: true}, {version: true}},
			revertTo: nil,
			desc:     "not rolling back to stable job version 0: it was registered before the ACL policies bound to the job last changed",
		},
		{
			name:     "stable version after the binding",
			steps:    []step{{version: true, stable: true}, {bind: true}, {version: true, stable: true}, {version: true}},
			revertTo: pointer.Of(uint64(1)),
			desc:     "rolling back to job version 1",
		},
		{
			name: "stability flags moved back to the version before the binding",
			steps: []step{{version: true, stable: true}, {bind: true}, {version: true, stable: true},
				{unstable: []uint64{1}, stableAt: []uint64{0}}, {version: true}},
			revertTo: nil,
			desc:     "not rolling back to stable job version 0",
		},
		{
			name: "the bound policy changed after the stable version",
			steps: []step{{bind: true}, {version: true, stable: true}, {bind: true},
				{version: true}},
			revertTo: nil,
			desc:     "not rolling back to stable job version 0",
		},
		{
			name:     "no policy bound",
			steps:    []step{{version: true, stable: true}, {version: true}},
			revertTo: pointer.Of(uint64(0)),
			desc:     "rolling back to job version 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, m := defaultTestDeploymentWatcher(t)

			// The mock applies the update to the state, which gives the
			// reverted job a new version: record what the watcher sent.
			type update struct {
				desc     string
				revertTo *uint64
			}
			var mu sync.Mutex
			var updates []update
			m.On("UpdateDeploymentStatus", mocker.MatchedBy(func(u *structs.DeploymentStatusUpdateRequest) bool {
				mu.Lock()
				defer mu.Unlock()
				rec := update{desc: u.DeploymentUpdate.StatusDescription}
				if u.Job != nil {
					rec.revertTo = pointer.Of(u.Job.Version)
				}
				updates = append(updates, rec)
				return true
			})).Return(nil)

			job := mock.Job()
			job.TaskGroups[0].Update = structs.DefaultUpdateStrategy.Copy()
			job.TaskGroups[0].Update.AutoRevert = true
			job.TaskGroups[0].Update.ProgressDeadline = 0
			policy := mock.ACLPolicy()
			policy.JobACL = &structs.JobACL{Namespace: job.Namespace, JobID: job.ID}

			versions := 0
			for _, s := range tc.steps {
				if s.bind {
					policy.Description += " changed"
					policy.SetHash()
					stored := *policy
					must.NoError(t, m.state.UpsertACLPolicies(structs.MsgTypeTestSetup, m.nextIndex(),
						[]*structs.ACLPolicy{&stored}))
				}
				if s.version {
					j := job.Copy()
					j.Stable = s.stable
					j.Meta["version"] = string(rune('a' + versions))
					must.NoError(t, m.state.UpsertJob(structs.MsgTypeTestSetup, m.nextIndex(), nil, j))
					versions++
				}
				for _, v := range s.unstable {
					must.NoError(t, m.state.UpdateJobStability(m.nextIndex(), job.Namespace, job.ID, v, false))
				}
				for _, v := range s.stableAt {
					must.NoError(t, m.state.UpdateJobStability(m.nextIndex(), job.Namespace, job.ID, v, true))
				}
			}

			d := mock.Deployment()
			d.JobID = job.ID
			d.JobVersion = uint64(versions - 1)
			d.TaskGroups["web"].AutoRevert = true
			must.NoError(t, m.state.UpsertDeployment(m.nextIndex(), d))

			w.SetEnabled(true, m.state)
			testutil.WaitForResult(func() (bool, error) { return watchersCount(w) == 1, nil },
				func(err error) { t.Fatalf("expected 1 deployment watcher, have %d", watchersCount(w)) })

			var resp structs.DeploymentUpdateResponse
			must.NoError(t, w.FailDeployment(&structs.DeploymentFailRequest{DeploymentID: d.ID}, &resp))

			mu.Lock()
			defer mu.Unlock()
			must.SliceNotEmpty(t, updates)
			last := updates[len(updates)-1]
			must.StrContains(t, last.desc, tc.desc)
			must.Eq(t, tc.revertTo, last.revertTo)
			// RevertedJobVersion names the version the revert registers.
			must.Eq(t, tc.revertTo == nil, resp.RevertedJobVersion == nil)
		})
	}
}
