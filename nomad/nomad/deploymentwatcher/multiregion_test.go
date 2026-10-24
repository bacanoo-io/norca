// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package deploymentwatcher

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
	mocker "github.com/stretchr/testify/mock"
)

// fakePeers simulates the deployments of the peer regions of a multiregion
// deployment behind the DeploymentRPC and JobRPC interfaces.
type fakePeers struct {
	l           sync.Mutex
	deployments map[string]*structs.Deployment
	unreachable map[string]bool
	calls       []string
}

func (f *fakePeers) LatestDeployment(args *structs.JobSpecificRequest, reply *structs.SingleDeploymentResponse) error {
	f.l.Lock()
	defer f.l.Unlock()
	if f.unreachable[args.Region] {
		return structs.ErrNoRegionPath
	}
	reply.Deployment = f.deployments[args.Region].Copy()
	return nil
}

func (f *fakePeers) set(region, status string) error {
	if f.unreachable[region] {
		return structs.ErrNoRegionPath
	}
	d := f.deployments[region]
	if d == nil {
		return fmt.Errorf("no deployment in region %q", region)
	}
	d.Status = status
	return nil
}

func (f *fakePeers) Run(args *structs.DeploymentRunRequest, _ *structs.DeploymentUpdateResponse) error {
	f.l.Lock()
	defer f.l.Unlock()
	f.calls = append(f.calls, "run "+args.Region)
	return f.set(args.Region, structs.DeploymentStatusRunning)
}

func (f *fakePeers) Unblock(args *structs.DeploymentUnblockRequest, _ *structs.DeploymentUpdateResponse) error {
	f.l.Lock()
	defer f.l.Unlock()
	f.calls = append(f.calls, "unblock "+args.Region)
	return f.set(args.Region, structs.DeploymentStatusSuccessful)
}

func (f *fakePeers) Fail(args *structs.DeploymentFailRequest, _ *structs.DeploymentUpdateResponse) error {
	f.l.Lock()
	defer f.l.Unlock()
	f.calls = append(f.calls, "fail "+args.Region)
	return f.set(args.Region, structs.DeploymentStatusFailed)
}

func (f *fakePeers) setStatus(region, status string) {
	f.l.Lock()
	defer f.l.Unlock()
	f.deployments[region].Status = status
}

func (f *fakePeers) setUnreachable(region string, unreachable bool) {
	f.l.Lock()
	defer f.l.Unlock()
	f.unreachable[region] = unreachable
}

func (f *fakePeers) getCalls() []string {
	f.l.Lock()
	defer f.l.Unlock()
	return append([]string(nil), f.calls...)
}

// testMultiregionWatcher watches the deployment of the region "gcp" of a job
// deployed to "aws", "gcp" and "hetzner" in that order.
func testMultiregionWatcher(t *testing.T, onFailure, status string, peers map[string]string) (*Watcher, *mockBackend, *fakePeers, *structs.Deployment) {
	m := newMockBackend(t)
	m.On("UpdateDeploymentStatus", mocker.Anything).Return(nil)

	job := mock.MultiregionJob()
	job.Region = "gcp"
	job.Multiregion.Strategy.OnFailure = onFailure
	job.Multiregion.Regions = []*structs.MultiregionRegion{{Name: "aws"}, {Name: "gcp"}, {Name: "hetzner"}}
	must.NoError(t, m.state.UpsertJob(structs.MsgTypeTestSetup, m.nextIndex(), nil, job))
	job, err := m.state.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)

	d := structs.NewMultiregionDeployment(job, 50)
	d.Status = status
	must.NoError(t, m.state.UpsertDeployment(m.nextIndex(), d))

	fake := &fakePeers{deployments: map[string]*structs.Deployment{}, unreachable: map[string]bool{}}
	for region, status := range peers {
		peer := structs.NewMultiregionDeployment(job, 50)
		peer.Status = status
		fake.deployments[region] = peer
	}

	w := NewDeploymentsWatcher(testlog.HCLogger(t), m, fake, fake,
		LimitStateQueriesPerSecond, CrossDeploymentUpdateBatchDuration)
	w.SetMultiregionPollInterval(10*time.Millisecond, 50*time.Millisecond)
	w.SetEnabled(true, m.state)
	t.Cleanup(func() { w.SetEnabled(false, nil) })
	return w, m, fake, d
}

func waitDeploymentStatus(t *testing.T, m *mockBackend, id, status string) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		d, err := m.state.DeploymentByID(nil, id)
		if err != nil {
			return err
		}
		if d.Status != status {
			return fmt.Errorf("deployment is %s, want %s", d.Status, status)
		}
		return nil
	}), wait.Timeout(5*time.Second), wait.Gap(10*time.Millisecond)))
}

func TestMultiregion_PendingStartsAfterPredecessor(t *testing.T) {
	ci.Parallel(t)

	// max_parallel = 1: gcp waits for aws.
	_, m, fake, d := testMultiregionWatcher(t, "", structs.DeploymentStatusPending, map[string]string{
		"aws":     structs.DeploymentStatusRunning,
		"hetzner": structs.DeploymentStatusPending,
	})

	time.Sleep(200 * time.Millisecond)
	cur, err := m.state.DeploymentByID(nil, d.ID)
	must.NoError(t, err)
	must.Eq(t, structs.DeploymentStatusPending, cur.Status)

	fake.setStatus("aws", structs.DeploymentStatusBlocked)
	waitDeploymentStatus(t, m, d.ID, structs.DeploymentStatusRunning)
	must.SliceEmpty(t, fake.getCalls())
}

func TestMultiregion_BlockedHandsOverAndUnblocks(t *testing.T) {
	ci.Parallel(t)

	_, m, fake, d := testMultiregionWatcher(t, "", structs.DeploymentStatusBlocked, map[string]string{
		"aws":     structs.DeploymentStatusBlocked,
		"hetzner": structs.DeploymentStatusPending,
	})

	// gcp runs the next region.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		calls := fake.getCalls()
		return len(calls) == 1 && calls[0] == "run hetzner"
	}), wait.Timeout(5*time.Second), wait.Gap(10*time.Millisecond)))

	// Once all regions are blocked, they are unblocked.
	fake.setStatus("hetzner", structs.DeploymentStatusBlocked)
	waitDeploymentStatus(t, m, d.ID, structs.DeploymentStatusSuccessful)
	must.SliceContainsAll(t, []string{"run hetzner", "unblock aws", "unblock hetzner"}, fake.getCalls())
}

func TestMultiregion_PartitionKeepsBlocked(t *testing.T) {
	ci.Parallel(t)

	_, m, fake, d := testMultiregionWatcher(t, "", structs.DeploymentStatusBlocked, map[string]string{
		"aws":     structs.DeploymentStatusBlocked,
		"hetzner": structs.DeploymentStatusBlocked,
	})
	fake.setUnreachable("hetzner", true)

	time.Sleep(200 * time.Millisecond)
	cur, err := m.state.DeploymentByID(nil, d.ID)
	must.NoError(t, err)
	must.Eq(t, structs.DeploymentStatusBlocked, cur.Status)

	fake.setUnreachable("hetzner", false)
	waitDeploymentStatus(t, m, d.ID, structs.DeploymentStatusSuccessful)
}

func TestMultiregion_FailurePolicies(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name      string
		onFailure string
		peers     map[string]string
		want      string
	}{
		{
			name:      "fail_all fails on a later failure",
			onFailure: structs.MultiregionOnFailureFailAll,
			peers:     map[string]string{"aws": "blocked", "hetzner": "failed"},
			want:      structs.DeploymentStatusFailed,
		},
		{
			name:  "default fails on an earlier failure",
			peers: map[string]string{"aws": "failed", "hetzner": "pending"},
			want:  structs.DeploymentStatusFailed,
		},
		{
			name:  "default ignores a later failure",
			peers: map[string]string{"aws": "blocked", "hetzner": "failed"},
			want:  structs.DeploymentStatusBlocked,
		},
		{
			name:      "fail_local ignores failures",
			onFailure: structs.MultiregionOnFailureFailLocal,
			peers:     map[string]string{"aws": "failed", "hetzner": "blocked"},
			want:      structs.DeploymentStatusBlocked,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m, _, d := testMultiregionWatcher(t, tc.onFailure, structs.DeploymentStatusBlocked, tc.peers)
			if tc.want == structs.DeploymentStatusFailed {
				waitDeploymentStatus(t, m, d.ID, tc.want)
				cur, err := m.state.DeploymentByID(nil, d.ID)
				must.NoError(t, err)
				must.Eq(t, structs.DeploymentStatusDescriptionFailedByPeer, cur.StatusDescription)
				return
			}
			time.Sleep(200 * time.Millisecond)
			cur, err := m.state.DeploymentByID(nil, d.ID)
			must.NoError(t, err)
			must.Eq(t, tc.want, cur.Status)
		})
	}
}

func TestMultiregion_RunUnblockStates(t *testing.T) {
	ci.Parallel(t)

	w, m, _, d := testMultiregionWatcher(t, "", structs.DeploymentStatusInitializing, map[string]string{})

	var resp structs.DeploymentUpdateResponse
	err := w.RunDeployment(&structs.DeploymentRunRequest{DeploymentID: d.ID}, &resp)
	must.ErrorContains(t, err, "is initializing")
	err = w.UnblockDeployment(&structs.DeploymentUnblockRequest{DeploymentID: d.ID}, &resp)
	must.ErrorContains(t, err, "only a blocked deployment can be unblocked")

	u := &structs.DeploymentStatusUpdateRequest{DeploymentUpdate: &structs.DeploymentStatusUpdate{
		DeploymentID: d.ID, Status: structs.DeploymentStatusPending,
	}}
	must.NoError(t, m.state.UpdateDeploymentStatus(structs.MsgTypeTestSetup, m.nextIndex(), u))

	// The peers have no deployment, so the watcher does not start it, but an
	// operator can.
	must.NoError(t, w.RunDeployment(&structs.DeploymentRunRequest{DeploymentID: d.ID}, &resp))
	must.NotEq(t, "", resp.EvalID)
	waitDeploymentStatus(t, m, d.ID, structs.DeploymentStatusRunning)

	err = w.RunDeployment(&structs.DeploymentRunRequest{DeploymentID: d.ID}, &resp)
	must.ErrorContains(t, err, "only a pending deployment can be run")
}
