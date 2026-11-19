// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

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
	"github.com/shoenig/test/wait"
)

// interruptedByLeaderChange is the error failInterrupted records.
const interruptedByLeaderChange = "the operation was interrupted by a leader change"

// markInterrupted marks a release with a running operation failed, as
// failInterrupted on a new leader does.
func (c *releaseTestClient) markInterrupted(ns, name string) {
	c.t.Helper()
	snap, err := c.srv.State().Snapshot()
	must.NoError(c.t, err)
	obj, rel, err := releaseObject(snap, ns, name)
	must.NoError(c.t, err)
	must.NotNil(c.t, rel)
	rel.Status.Phase = structs.ReleasePhaseFailed
	rel.Status.LastOperation.Error = interruptedByLeaderChange
	rel.Status.LastOperation.EndTime = nowRFC3339()
	_, err = c.srv.releases.writeRelease(rel, obj.ModifyIndex)
	must.NoError(c.t, err)
}

// An operation whose release a new leader marked failed (failInterrupted)
// stops at its next write: it writes no further resource, restores
// nothing, leaves the status the new leader wrote and answers that a
// leader change interrupted it (review finding ATM-12). It went on writing
// and recorded its own result over the failed status.
func TestRelease_InterruptedOperationStopsWriting(t *testing.T) {
	ci.Parallel(t)
	c, during := newReleaseRaceTest(t, "default/fenced", "namespace//fenced-b")
	interrupt := func() { c.markInterrupted("default", "fenced") }
	during.Store(&interrupt)

	_, err := c.apply("default", "fenced", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "fenced"}, Atomic: pointer.Of(true),
		Resources: []*structs.ReleaseResource{
			releaseNamespace("fenced-a", "v1"), releaseNamespace("fenced-b", "v1"), releaseNamespace("fenced-c", "v1"),
		},
	})
	must.Nil(t, during.Load(), must.Sprint("the operation did not reach the second namespace"))
	must.ErrorContains(t, err, interruptedByLeaderChange)

	must.Eq(t, "<none>", c.namespaceDescription("fenced-b"), must.Sprint("the operation wrote after it was interrupted"))
	must.Eq(t, "<none>", c.namespaceDescription("fenced-c"), must.Sprint("the operation wrote after it was interrupted"))
	must.Eq(t, "v1", c.namespaceDescription("fenced-a"), must.Sprint("the interrupted operation ran its restore"))

	rel := c.get("default", "fenced")
	must.Eq(t, structs.ReleasePhaseFailed, rel.Status.Phase)
	must.Eq(t, interruptedByLeaderChange, rel.Status.LastOperation.Error,
		must.Sprint("the interrupted operation wrote over the status of the new leader"))
}

// An uninstall whose release a new leader marked failed stops before its
// next deletion: it deletes no further resource and neither the revisions
// nor the release (ATM-12).
func TestRelease_InterruptedUninstallStopsDeleting(t *testing.T) {
	ci.Parallel(t)
	var c *releaseTestClient
	fired := false
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.releaseBeforeDelete = func(rel string, ref *structs.ReleaseResourceRef) {
			if rel == "default/unfenced" && !fired {
				fired = true
				c.markInterrupted("default", "unfenced")
			}
		}
	})
	defer cleanup()

	_, err := c.apply("default", "unfenced", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "unfenced"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{releaseNamespace("unfenced-a", "v1"), releaseNamespace("unfenced-b", "v1")},
	})
	must.NoError(t, err)

	_, err = c.uninstall("default", "unfenced", "")
	must.True(t, fired, must.Sprint("the uninstall deleted nothing"))
	must.ErrorContains(t, err, interruptedByLeaderChange)
	must.Eq(t, "v1", c.namespaceDescription("unfenced-a"), must.Sprint("the uninstall deleted after it was interrupted"))
	must.Eq(t, "v1", c.namespaceDescription("unfenced-b"), must.Sprint("the uninstall deleted after it was interrupted"))
	rel := c.get("default", "unfenced")
	must.NotNil(t, rel, must.Sprint("the interrupted uninstall deleted the release"))
	must.Eq(t, structs.ReleasePhaseFailed, rel.Status.Phase)
	must.Eq(t, interruptedByLeaderChange, rel.Status.LastOperation.Error)
	must.SliceNotEmpty(t, c.history("default", "unfenced"), must.Sprint("the interrupted uninstall deleted the revisions"))
}

// An operation runs with a context of the leadership: when the leadership
// ends (here it is established again, as after a snapshot restore) the
// operation stops writing, restores nothing and answers that a leader
// change interrupted it; the leadership established next marks the release
// failed once the operation has stopped, and the release can be operated on
// again (ATM-12). The operation was not tied to the leadership.
func TestRelease_LeadershipEndStopsOperation(t *testing.T) {
	ci.Parallel(t)
	c, during := newReleaseRaceTest(t, "default/term", "namespace//term-b")
	reassert := func() {
		errCh := make(chan error, 1)
		c.srv.reassertLeaderCh <- errCh
		must.NoError(t, <-errCh)
	}
	during.Store(&reassert)

	resources := []*structs.ReleaseResource{
		releaseNamespace("term-a", "v1"), releaseNamespace("term-b", "v1"), releaseNamespace("term-c", "v1"),
	}
	_, err := c.apply("default", "term", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "term"}, Atomic: pointer.Of(true), Resources: resources,
	})
	must.Nil(t, during.Load(), must.Sprint("the operation did not reach the second namespace"))
	must.ErrorContains(t, err, interruptedByLeaderChange)
	must.Eq(t, "<none>", c.namespaceDescription("term-b"), must.Sprint("the operation wrote after the leadership ended"))
	must.Eq(t, "<none>", c.namespaceDescription("term-c"), must.Sprint("the operation wrote after the leadership ended"))
	must.Eq(t, "v1", c.namespaceDescription("term-a"), must.Sprint("the interrupted operation ran its restore"))

	testutil.WaitForLeader(t, c.srv.RPC)
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		rel := c.get("default", "term")
		if rel.Status.Phase != structs.ReleasePhaseFailed || rel.Status.LastOperation.Error != interruptedByLeaderChange {
			return fmt.Errorf("release is %s: %q", rel.Status.Phase, rel.Status.LastOperation.Error)
		}
		return nil
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	_, err = c.apply("default", "term", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "term"}, Atomic: pointer.Of(true), Resources: resources,
	})
	must.NoError(t, err)
	must.Eq(t, "v1", c.namespaceDescription("term-c"))
}

// An operation waiting for a hook job stops waiting when the leadership
// ends: it answers that a leader change interrupted it and leaves the hook
// job, which an interrupted operation does not purge (ATM-12). It waited
// until the hook timed out and purged the job.
func TestRelease_LeadershipEndStopsHookWait(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	hook := mock.BatchJob()
	hook.ID, hook.Name, hook.Namespace = "wait-hook", "wait-hook", ""
	hook.Meta = map[string]string{
		structs.ReleaseMetaHook:        structs.ReleaseHookPreInstall,
		structs.ReleaseMetaHookTimeout: "20s",
	}
	live := func() *structs.Job {
		job, err := c.srv.State().JobByID(nil, "default", "wait-hook")
		must.NoError(t, err)
		return job
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := c.apply("default", "waiting", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "waiting"}, Atomic: pointer.Of(true),
			Resources: []*structs.ReleaseResource{
				{Type: structs.ReleaseResourceJob, Job: testReleaseJob("waiting-web")},
				{Type: structs.ReleaseResourceJob, Job: hook},
			},
		})
		done <- err
	}()

	// The hook runs (it can't finish without nodes) when the leadership is
	// established again.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return jobOfRelease(live(), "default", "waiting")
	}), wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))
	errCh := make(chan error, 1)
	c.srv.reassertLeaderCh <- errCh
	must.NoError(t, <-errCh)

	err := <-done
	must.ErrorContains(t, err, interruptedByLeaderChange)
	must.Less(t, 15*time.Second, time.Since(start), must.Sprint("the operation waited for the hook after the leadership ended"))
	must.NotNil(t, live(), must.Sprint("the interrupted operation purged the hook job"))
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		rel := c.get("default", "waiting")
		if rel.Status.Phase != structs.ReleasePhaseFailed {
			return fmt.Errorf("release is %s", rel.Status.Phase)
		}
		return nil
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
}

// A server that is not the leader starts no release operation: it has no
// leadership context to run it with (ATM-12).
func TestRelease_OperationNeedsLeadership(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	m := c.srv.releases
	m.setLeader(false)
	_, _, err := m.newOp(nil, c.token, "default", "app", 0)
	must.ErrorContains(t, err, "not the leader")

	m.setLeader(true)
	op, cancel, err := m.newOp(nil, c.token, "default", "app", 0)
	must.NoError(t, err)
	must.NoError(t, op.ctx.Err())
	m.setLeader(false)
	must.ErrorIs(t, op.interruption(), errReleaseInterrupted)
	cancel()
	m.setLeader(true)
}
