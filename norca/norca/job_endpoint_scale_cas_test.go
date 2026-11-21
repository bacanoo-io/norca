// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/admission"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// scaleGate holds the next Job.Scale in its validating admission webhook
// call: the window between the snapshot of Job.Scale and its Raft apply.
type scaleGate struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (g *scaleGate) arm() (entered, release chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.entered = make(chan struct{}, 1)
	g.release = make(chan struct{})
	return g.entered, g.release
}

func (g *scaleGate) hold(op string) {
	g.mu.Lock()
	if !g.armed || op != structs.AdmissionOperationScale {
		g.mu.Unlock()
		return
	}
	g.armed = false
	entered, release := g.entered, g.release
	g.mu.Unlock()
	entered <- struct{}{}
	select {
	case <-release:
	case <-time.After(20 * time.Second):
	}
}

// Job.Scale checks the policy bounds, the deployment block and the previous
// count on its snapshot; the scaling is applied only to that job (review
// finding ASC-8). A validating admission webhook holds the scaling between
// its snapshot and its Raft apply while the job changes.
func TestJobScale_AppliedOnTheJobItWasCheckedOn(t *testing.T) {
	ci.Parallel(t)

	gate := &scaleGate{}
	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		gate.hold(req.Operation)
		return &admission.ReviewResponse{Allowed: true}
	})
	s, cleanup := admissionServer(t, &config.AdmissionWebhookConfig{
		Name: "slow", URL: hook.URL, Timeout: 30 * time.Second,
	})
	defer cleanup()
	state := s.State()

	newJob := func() *structs.Job {
		job, _ := mock.JobWithScalingPolicy()
		tg := job.TaskGroups[0]
		tg.Count = 5
		tg.Scaling.Min, tg.Scaling.Max = 1, 10
		tg.Scaling.Engine = structs.ScalingPolicyEngineExternal
		_, err := admissionRegisterJob(t, s, job, "")
		must.NoError(t, err)
		return job
	}
	scaleAsync := func(job *structs.Job, count int64) chan error {
		done := make(chan error, 1)
		go func() {
			var resp structs.JobRegisterResponse
			done <- msgpackrpc.CallWithCodec(rpcClient(t, s), "Job.Scale", scaleRequest(job, count), &resp)
		}()
		return done
	}
	waitEntered := func(entered chan struct{}) {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("Job.Scale did not reach the webhook")
		}
	}
	registerV1 := func(job *structs.Job, max int64) *structs.Job {
		v1 := job.Copy()
		v1.Meta["v"] = "1"
		v1.TaskGroups[0].Scaling.Max = max
		_, err := admissionRegisterJob(t, s, v1, "")
		must.NoError(t, err)
		stored, err := state.JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		must.Eq(t, uint64(1), stored.Version)
		return stored
	}

	t.Run("count outside the bounds of the version registered in between", func(t *testing.T) {
		job := newJob()
		entered, release := gate.arm()
		done := scaleAsync(job, 9) // within max 10 of v0

		waitEntered(entered)
		v1 := registerV1(job, 6) // v1 allows at most 6; it keeps the count 5
		must.Eq(t, 5, v1.TaskGroups[0].Count)
		close(release)
		err := <-done

		stored, err2 := state.JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err2)
		tg := stored.TaskGroups[0]
		t.Logf("scale error: %v; stored version %d, count %d, policy max %d",
			err, stored.Version, tg.Count, tg.Scaling.Max)
		if err == nil && int64(tg.Count) > tg.Scaling.Max {
			t.Errorf("the scale was applied past the bounds of the stored version: count %d > max %d",
				tg.Count, tg.Scaling.Max)
		}
	})

	t.Run("scale down applied during the active deployment of the new version", func(t *testing.T) {
		job := newJob()
		entered, release := gate.arm()
		done := scaleAsync(job, 2) // no deployment on the snapshot

		waitEntered(entered)
		v1 := registerV1(job, 10)
		// The scheduler starts the deployment of v1 (no scheduler runs in
		// this server: it is written as the plan applier would).
		d := structs.NewDeployment(v1, 50)
		d.TaskGroups[v1.TaskGroups[0].Name] = &structs.DeploymentState{DesiredTotal: 5}
		idx, err := state.LatestIndex()
		must.NoError(t, err)
		must.NoError(t, state.UpsertDeployment(idx+1, d))
		close(release)
		err = <-done

		stored, err2 := state.JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err2)
		dep, err2 := state.DeploymentByID(nil, d.ID)
		must.NoError(t, err2)
		t.Logf("scale error: %v; stored version %d, count %d; deployment of v%d %s",
			err, stored.Version, stored.TaskGroups[0].Count, dep.JobVersion, dep.Status)
		if err == nil && dep.Active() && stored.TaskGroups[0].Count == 2 {
			t.Errorf("the scale down was applied during the active deployment of version %d", dep.JobVersion)
		}

		// Direct comparison: the same request now is refused.
		var resp structs.JobRegisterResponse
		err = msgpackrpc.CallWithCodec(rpcClient(t, s), "Job.Scale", scaleRequest(job, 1), &resp)
		t.Logf("the same kind of request made after the deployment started: %v", err)
	})

	t.Run("previous count of the scaling event", func(t *testing.T) {
		job := newJob()
		entered, release := gate.arm()
		doneA := scaleAsync(job, 9) // the autoscaler: 5 -> 9

		waitEntered(entered)
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s), "Job.Scale", scaleRequest(job, 2), &resp)) // operator: 5 -> 2
		close(release)
		// The scaling checked on 5 is refused: the count is 2 now.
		errA := <-doneA
		must.ErrorContains(t, errA, "changed while it was scaled")

		stored, err := state.JobByID(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		events, _, err := state.ScalingEventsByJob(nil, job.Namespace, job.ID)
		must.NoError(t, err)
		evs := events[job.TaskGroups[0].Name]
		for _, e := range evs { // newest first
			t.Logf("event: previous %d -> count %d", e.PreviousCount, *e.Count)
		}
		t.Logf("stored count %d", stored.TaskGroups[0].Count)
		must.SliceNotEmpty(t, evs)
		must.Eq(t, 2, stored.TaskGroups[0].Count)
		for _, e := range evs {
			must.False(t, e.Count != nil && *e.Count == 9, must.Sprint("the refused scaling left an event"))
		}
	})
}
