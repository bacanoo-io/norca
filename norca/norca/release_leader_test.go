// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
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

// startOperationElsewhere records on a release the start of an operation on
// another leader, by the user and in the second of the last operation
// recorded: the phase and the last operation that operation writes.
func (c *releaseTestClient) startOperationElsewhere(ns, name, phase, action string) {
	c.t.Helper()
	snap, err := c.srv.State().Snapshot()
	must.NoError(c.t, err)
	obj, rel, err := releaseObject(snap, ns, name)
	must.NoError(c.t, err)
	must.NotNil(c.t, rel)
	last := rel.Status.LastOperation
	rel.Status.Phase = phase
	rel.Status.LastOperation = &structs.ReleaseOperation{
		ID: uuid.Generate(), Action: action, Revision: last.Revision, User: last.User, StartTime: last.StartTime,
	}
	_, err = c.srv.releases.writeRelease(rel, obj.ModifyIndex)
	must.NoError(c.t, err)
}

// newReleaseRevisionRace returns a release test client whose release
// operations run during, once, right before they write a revision (or with
// deleting delete one) for which match reports true; fired reports whether
// it ran.
func newReleaseRevisionRace(t *testing.T, match func(rev *structs.ReleaseRevision, deleting bool) bool,
	during func(c *releaseTestClient)) (c *releaseTestClient, fired *atomic.Bool) {

	t.Helper()
	fired = &atomic.Bool{}
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.releaseBeforeRevision = func(_ string, rev *structs.ReleaseRevision, deleting bool) {
			if match(rev, deleting) && fired.CompareAndSwap(false, true) {
				during(c)
			}
		}
	})
	t.Cleanup(cleanup)
	return c, fired
}

// An operation that a leader change interrupted right before it records
// the result in its revision writes no revision: the new revision stays
// pending and the previous one is not superseded (review finding ATM-16).
// The revision writes were checked for an interruption once, before the
// result was put together, and then the revisions were written anyway.
func TestRelease_InterruptedOperationWritesNoRevision(t *testing.T) {
	ci.Parallel(t)
	cases := []struct {
		name string
		// upgrade fails with the bad job; result is the phase of the revision
		// the operation records.
		bad    bool
		result string
	}{
		{name: "deployed", result: structs.ReleasePhaseDeployed},
		{name: "failed", bad: true, result: structs.ReleasePhaseFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fired := newReleaseRevisionRace(t, func(rev *structs.ReleaseRevision, deleting bool) bool {
				return !deleting && rev.Revision == 2 && rev.Status.Phase == tc.result
			}, func(c *releaseTestClient) { c.markInterrupted("default", "revs") })

			pack := func(desc string) []*structs.ReleaseResource {
				job := testReleaseJob("revs-job")
				if tc.bad && desc == "v2" {
					job.TaskGroups[0].Tasks[0].Resources.CPU = -5
				}
				return []*structs.ReleaseResource{releaseNamespace("revs-a", desc), {Type: structs.ReleaseResourceJob, Job: job}}
			}
			_, err := c.apply("default", "revs", &structs.ReleaseApplyRequest{
				Pack: &structs.ReleasePack{Name: "revs"}, Atomic: pointer.Of(false), Resources: pack("v1"),
			})
			must.NoError(t, err)

			_, err = c.apply("default", "revs", &structs.ReleaseApplyRequest{
				Pack: &structs.ReleasePack{Name: "revs"}, Atomic: pointer.Of(false), Resources: pack("v2"),
			})
			must.True(t, fired.Load(), must.Sprint("the operation wrote no revision result"))
			must.ErrorContains(t, err, interruptedByLeaderChange)
			must.Eq(t, structs.ReleasePhasePending, c.revision("default", "revs", 2).Status.Phase,
				must.Sprint("the interrupted operation recorded its result in the revision"))
			must.Eq(t, structs.ReleasePhaseDeployed, c.revision("default", "revs", 1).Status.Phase,
				must.Sprint("the interrupted operation superseded the previous revision"))
			must.Eq(t, interruptedByLeaderChange, c.get("default", "revs").Status.LastOperation.Error)
		})
	}
}

// An uninstall that a leader change interrupted while it deletes the
// revisions deletes no further revision, and not the release (ATM-16). The
// revisions were all deleted after a single check.
func TestRelease_InterruptedUninstallKeepsRevisions(t *testing.T) {
	ci.Parallel(t)
	c, fired := newReleaseRevisionRace(t, func(_ *structs.ReleaseRevision, deleting bool) bool { return deleting },
		func(c *releaseTestClient) { c.markInterrupted("default", "kept") })

	for _, desc := range []string{"v1", "v2"} {
		_, err := c.apply("default", "kept", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "kept"}, Resources: []*structs.ReleaseResource{releaseNamespace("kept-a", desc)},
		})
		must.NoError(t, err)
	}

	_, err := c.uninstall("default", "kept", "")
	must.True(t, fired.Load(), must.Sprint("the uninstall deleted no revision"))
	must.ErrorContains(t, err, interruptedByLeaderChange)
	must.Len(t, 2, c.history("default", "kept"), must.Sprint("the interrupted uninstall deleted revisions"))
	rel := c.get("default", "kept")
	must.NotNil(t, rel)
	must.Eq(t, interruptedByLeaderChange, rel.Status.LastOperation.Error)
}

// An uninstall deletes a revision only as it read it: one written since is
// left, and the uninstall fails (ATM-16). The revisions were deleted
// without check-and-set.
func TestRelease_UninstallDeletesRevisionsAsRead(t *testing.T) {
	ci.Parallel(t)
	c, fired := newReleaseRevisionRace(t, func(rev *structs.ReleaseRevision, deleting bool) bool {
		return deleting && rev.Revision == 1
	}, func(c *releaseTestClient) {
		// A stale writer changes revision 2.
		snap, err := c.srv.State().Snapshot()
		must.NoError(c.t, err)
		rev, _, err := releaseRevision(snap, "default", "asread", 2)
		must.NoError(c.t, err)
		_, rel, err := releaseObject(snap, "default", "asread")
		must.NoError(c.t, err)
		rev.Status.Message = "written meanwhile"
		_, err = c.srv.releases.writeRevision(rev, rel, rev.ModifyIndex)
		must.NoError(c.t, err)
	})

	for _, desc := range []string{"v1", "v2"} {
		_, err := c.apply("default", "asread", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "asread"}, Resources: []*structs.ReleaseResource{releaseNamespace("asread-a", desc)},
		})
		must.NoError(t, err)
	}

	_, err := c.uninstall("default", "asread", "")
	must.True(t, fired.Load(), must.Sprint("the uninstall deleted no revision"))
	must.ErrorContains(t, err, "modify index")
	hist := c.history("default", "asread")
	must.Len(t, 1, hist, must.Sprint("the uninstall deleted a revision written since it read it"))
	must.Eq(t, "written meanwhile", hist[0].Status.Message)
	must.Eq(t, structs.ReleasePhaseFailed, c.get("default", "asread").Status.Phase)
}

// An operation that a leader change interrupted after it recorded its
// result prunes no revision (ATM-16): an operation started on the new
// leader works with them. The revisions were pruned without a check.
func TestRelease_InterruptedOperationPrunesNothing(t *testing.T) {
	ci.Parallel(t)
	c, fired := newReleaseRevisionRace(t, func(rev *structs.ReleaseRevision, deleting bool) bool {
		return deleting && rev.Revision == 1
	}, func(c *releaseTestClient) {
		c.startOperationElsewhere("default", "pruned", structs.ReleasePhaseApplying, structs.ReleaseActionUpgrade)
	})

	for _, desc := range []string{"v1", "v2"} {
		_, err := c.apply("default", "pruned", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "pruned"}, MaxHistory: 1,
			Resources: []*structs.ReleaseResource{releaseNamespace("pruned-a", desc)},
		})
		must.NoError(t, err)
	}
	must.True(t, fired.Load(), must.Sprint("the operation pruned no revision"))
	must.Len(t, 2, c.history("default", "pruned"), must.Sprint("the interrupted operation pruned a revision"))
}

// An operation that a leader change interrupted after it recorded its
// result takes no resources off the releases that owned them (review
// finding ATM-18): it answers with a warning, and an operation started on
// the new leader decides. The other releases were written without a check.
func TestRelease_InterruptedOperationTakesNothingOver(t *testing.T) {
	ci.Parallel(t)
	var c *releaseTestClient
	fired := &atomic.Bool{}
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.releaseBeforeTakeOver = func(rel, _ string) {
			if rel == "default/taker" && fired.CompareAndSwap(false, true) {
				c.startOperationElsewhere("default", "taker", structs.ReleasePhaseApplying, structs.ReleaseActionUpgrade)
			}
		}
	})
	defer cleanup()

	res := func() []*structs.ReleaseResource {
		return []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob("taken")}}
	}
	_, err := c.apply("default", "owner", &structs.ReleaseApplyRequest{Pack: &structs.ReleasePack{Name: "owner"}, Resources: res()})
	must.NoError(t, err)

	resp, err := c.apply("default", "taker", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "taker"}, Resources: res(), TakeOwnership: true,
	})
	must.NoError(t, err)
	must.True(t, fired.Load(), must.Sprint("the operation took nothing over"))
	must.Len(t, 1, c.get("default", "owner").Status.Resources,
		must.Sprint("the interrupted operation took resources off the release that owned them"))
	warned := false
	for _, w := range resp.Warnings {
		warned = warned || strings.Contains(w, `resources of release "default/owner" not taken over`)
	}
	must.True(t, warned, must.Sprintf("warnings: %q", resp.Warnings))
}

// An operation started on a new leader by the same user in the same second
// as an interrupted one is another operation: the interrupted one stops at
// its next write (review finding ATM-17). The fence compared the action,
// revision, user and times of the last operation, which were the same, so
// the interrupted uninstall went on deleting.
func TestRelease_OperationOfSameUserAndSecondIsAnother(t *testing.T) {
	ci.Parallel(t)
	var c *releaseTestClient
	fired := &atomic.Bool{}
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.releaseBeforeDelete = func(rel string, _ *structs.ReleaseResourceRef) {
			if rel == "default/twice" && fired.CompareAndSwap(false, true) {
				// The new leader marks the uninstall failed, and the user
				// uninstalls again there, in the same second.
				c.markInterrupted("default", "twice")
				c.startOperationElsewhere("default", "twice", structs.ReleasePhaseUninstalling, structs.ReleaseActionUninstall)
			}
		}
	})
	defer cleanup()

	_, err := c.apply("default", "twice", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "twice"}, Atomic: pointer.Of(false),
		Resources: []*structs.ReleaseResource{releaseNamespace("twice-a", "v1"), releaseNamespace("twice-b", "v1")},
	})
	must.NoError(t, err)
	must.NotEq(t, "", c.get("default", "twice").Status.LastOperation.ID, must.Sprint("the install recorded no operation ID"))

	_, err = c.uninstall("default", "twice", "")
	must.True(t, fired.Load(), must.Sprint("the uninstall deleted nothing"))
	must.ErrorContains(t, err, interruptedByLeaderChange)
	must.Eq(t, "v1", c.namespaceDescription("twice-a"), must.Sprint("the interrupted uninstall deleted"))
	must.Eq(t, "v1", c.namespaceDescription("twice-b"), must.Sprint("the interrupted uninstall deleted"))
	rel := c.get("default", "twice")
	must.NotNil(t, rel, must.Sprint("the interrupted uninstall deleted the release"))
	must.Eq(t, structs.ReleasePhaseUninstalling, rel.Status.Phase,
		must.Sprint("the interrupted uninstall wrote over the operation of the new leader"))
}

// The fence holds only for the operation of its ID; a release recorded
// without an ID holds only a fence taken without one, as a test run takes
// it from such a release (ATM-17).
func TestReleaseFence_OperationID(t *testing.T) {
	ci.Parallel(t)
	release := func(id string) *structs.Release {
		return &structs.Release{ID: "rel", Status: &structs.ReleaseStatus{
			Phase: structs.ReleasePhaseUninstalling,
			LastOperation: &structs.ReleaseOperation{ID: id, Action: structs.ReleaseActionUninstall,
				Revision: 1, User: "alice", StartTime: "2026-11-19T10:00:00Z"},
		}}
	}
	cases := []struct {
		name          string
		fence, stored string
		holds         bool
	}{
		{name: "same operation", fence: "op-1", stored: "op-1", holds: true},
		{name: "another operation in the same second", fence: "op-1", stored: "op-2"},
		{name: "recorded without an ID", fence: "op-1", stored: ""},
		{name: "taken without an ID", fence: "", stored: "op-2"},
		{name: "both without an ID", fence: "", stored: "", holds: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must.Eq(t, tc.holds, newReleaseFence(release(tc.fence)).holds(release(tc.stored)))
		})
	}
}

// A test run that a leader change interrupted answers that no result was
// recorded: the new leader does not mark a release failed for a test, its
// phase stays as it was. It answered that the new leader marks the release
// failed.
func TestRelease_InterruptedTestRecordsNoResult(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	hook := mock.BatchJob()
	hook.ID, hook.Name, hook.Namespace = "tested-hook", "tested-hook", ""
	hook.Meta = map[string]string{
		structs.ReleaseMetaHook:        structs.ReleaseHookTest,
		structs.ReleaseMetaHookTimeout: "20s",
	}
	_, err := c.apply("default", "tested", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "tested"},
		Resources: []*structs.ReleaseResource{
			releaseNamespace("tested-a", "v1"), {Type: structs.ReleaseResourceJob, Job: hook},
		},
	})
	must.NoError(t, err)

	done := make(chan error, 1)
	codec := rpcClient(t, c.srv)
	go func() {
		req := &structs.ReleaseTestRequest{Name: "tested",
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token}}
		var resp structs.ReleaseTestResponse
		done <- msgpackrpc.CallWithCodec(codec, "Release.Test", req, &resp)
	}()

	// The hook runs (it can't finish without nodes) when the leadership is
	// established again.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		job, err := c.srv.State().JobByID(nil, "default", "tested-hook")
		return err == nil && jobOfRelease(job, "default", "tested")
	}), wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))
	errCh := make(chan error, 1)
	c.srv.reassertLeaderCh <- errCh
	must.NoError(t, <-errCh)

	err = <-done
	must.ErrorContains(t, err, "the test was interrupted by a leader change; no result was recorded")
	testutil.WaitForLeader(t, c.srv.RPC)
	rel := c.get("default", "tested")
	must.Eq(t, structs.ReleasePhaseDeployed, rel.Status.Phase)
	must.Nil(t, rel.Status.LastTest)
}
