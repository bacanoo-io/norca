// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package deploymentwatcher

import (
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	mocker "github.com/stretchr/testify/mock"
)

// The auto revert of a failed deployment keeps the count that a scaling
// policy set during the deployment: before, it wrote the stable version back
// with the count stored on it, and a scale up under load was lost (review
// finding ASC-4).
func TestWatcher_AutoRevertKeepsScaledCount(t *testing.T) {
	ci.Parallel(t)
	w, m := testDeploymentWatcher(t, 1000.0, 1*time.Millisecond)

	m.On("UpdateDeploymentStatus", mocker.Anything).Return(nil)
	m.On("UpdateAllocDesiredTransition", mocker.Anything).Return(nil).Maybe()

	// v0: a group scaled by an enabled policy, the stable version.
	job, _ := mock.JobWithScalingPolicy()
	tg := job.TaskGroups[0]
	tg.Count = 3
	tg.Scaling.Min, tg.Scaling.Max = 1, 20
	tg.Update = structs.DefaultUpdateStrategy.Copy()
	tg.Update.AutoRevert = true
	tg.Update.ProgressDeadline = 0
	job.Stable = true
	must.NoError(t, m.state.UpsertJob(structs.MsgTypeTestSetup, m.nextIndex(), nil, job))

	// The policy scales v0 to 5 (Job.Scale applies state.ScaleJob): the
	// version and its stability are kept, the version entry gets the count.
	_, err := m.state.ScaleJob(structs.MsgTypeTestSetup, m.nextIndex(), job.Namespace, job.ID, tg.Name, 5, 0)
	must.NoError(t, err)
	v0, err := m.state.JobByIDAndVersion(nil, job.Namespace, job.ID, 0)
	must.NoError(t, err)
	must.True(t, v0.Stable)
	must.Eq(t, 5, v0.TaskGroups[0].Count)

	// v1: a new version is registered. Job.Register keeps the scaled count
	// (keepScaledCounts): 5.
	v1 := v0.Copy()
	v1.Stable = false
	v1.Meta["foo"] = "bar"
	must.NoError(t, m.state.UpsertJob(structs.MsgTypeTestSetup, m.nextIndex(), nil, v1))
	v1, err = m.state.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, uint64(1), v1.Version)
	must.Eq(t, 5, v1.TaskGroups[0].Count)

	// The deployment of v1 and one of its allocations.
	d := structs.NewDeployment(v1, 50)
	d.TaskGroups[tg.Name] = &structs.DeploymentState{AutoRevert: true, DesiredTotal: 5}
	must.NoError(t, m.state.UpsertDeployment(m.nextIndex(), d))
	a := mock.Alloc()
	a.JobID = v1.ID
	a.Job = v1
	a.TaskGroup = tg.Name
	a.DeploymentID = d.ID
	must.NoError(t, m.state.UpsertAllocs(structs.MsgTypeTestSetup, m.nextIndex(), []*structs.Allocation{a}))

	// Under load the built-in autoscaler scales v1 up during its deployment
	// (scale_during_deployment defaults to up_only for its own policies).
	_, err = m.state.ScaleJob(structs.MsgTypeTestSetup, m.nextIndex(), job.Namespace, job.ID, tg.Name, 9, 0)
	must.NoError(t, err)
	scaled, err := m.state.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Eq(t, uint64(1), scaled.Version)
	must.Eq(t, 9, scaled.TaskGroups[0].Count)

	w.SetEnabled(true, m.state)
	testutil.WaitForResult(func() (bool, error) { return watchersCount(w) == 1, nil },
		func(err error) { t.Fatalf("expected 1 deployment watcher, have %d", watchersCount(w)) })

	// An allocation of v1 reports unhealthy: the deployment fails and the
	// watcher reverts to the stable version v0 (the automatic path).
	a2 := a.Copy()
	a2.DeploymentStatus = &structs.AllocDeploymentStatus{Healthy: pointer.Of(false), Timestamp: time.Now()}
	must.NoError(t, m.state.UpdateAllocsFromClient(structs.MsgTypeTestSetup, m.nextIndex(), []*structs.Allocation{a2}))

	var reverted *structs.Job
	testutil.WaitForResult(func() (bool, error) {
		j, err := m.state.JobByID(nil, job.Namespace, job.ID)
		if err != nil {
			return false, err
		}
		if j.Version != 2 {
			return false, fmt.Errorf("job version %d, want the revert's 2", j.Version)
		}
		reverted = j
		return true, nil
	}, func(err error) { t.Fatal(err) })

	dep, err := m.state.DeploymentByID(nil, d.ID)
	must.NoError(t, err)
	t.Logf("deployment %s: %s", dep.Status, dep.StatusDescription)
	t.Logf("count before the failure: 9; after the auto revert (version %d): %d",
		reverted.Version, reverted.TaskGroups[0].Count)

	events, _, err := m.state.ScalingEventsByJob(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	t.Logf("scaling events of the group: %d", len(events[tg.Name]))

	must.Eq(t, 9, reverted.TaskGroups[0].Count,
		must.Sprint("the auto revert dropped the count scaled during the deployment"))
}
