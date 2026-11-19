// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Release operations and the leadership (review finding ATM-12).
//
// An operation runs in the RPC handler on the leader, with a context of the
// leadership term it started in. The term ends when the leadership is
// revoked (revokeLeadership: the leadership was lost, or it is established
// again after a snapshot restore), and the operation stops writing.
//
// A server may notice the loss of its leadership late, and the writes of an
// operation are forwarded to the new leader. So, like the evaluation token
// fences plans of an old leader, an operation also checks right before each
// write that its release in the state is still as the operation put it (its
// fence): the new leader changes it when it marks the release failed
// (failInterrupted), and so does an operation started there.
//
// An interrupted operation writes nothing more: no resource, no restore of
// an atomic operation, no purge of a hook job, no status. The release is
// marked failed by the leader of the next term (releaseLeaderLoop), once the
// operations of the terms before it have stopped on this server.

var (
	// errReleaseLeadershipEnded is the cause of the cancellation of the
	// context of a term.
	errReleaseLeadershipEnded = errors.New("the leadership ended")

	// errReleaseInterrupted is the error of a write of an operation that a
	// leader change interrupted.
	errReleaseInterrupted = errors.New("the operation was interrupted by a leader change; the new leader marks the release failed")

	// errReleaseTestInterrupted is the cause in the answer of a test run
	// that a leader change interrupted: no leader marks a release failed
	// for a test, its phase stays as it was.
	errReleaseTestInterrupted = errors.New("the test was interrupted by a leader change; no result was recorded")
)

// releaseTerm is a leadership term of the release manager.
type releaseTerm struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	// ops are the operations started in the term that still run.
	ops sync.WaitGroup

	// before are the terms that ended before this one began.
	before []*releaseTerm
}

// setLeader begins a leadership term (leader) or ends the current one. The
// operations of the term ended stop writing.
func (m *releaseManager) setLeader(leader bool) {
	m.lock.Lock()
	ended := m.term
	m.term = nil
	if ended != nil {
		m.ended = append(m.ended, ended)
	}
	if leader {
		ctx, cancel := context.WithCancelCause(context.Background())
		m.term = &releaseTerm{ctx: ctx, cancel: cancel, before: m.ended}
		m.ended = nil
	}
	m.lock.Unlock()
	if ended != nil {
		ended.cancel(errReleaseLeadershipEnded)
	}
}

// leadershipTerm returns the context of the current leadership term, nil on
// a server that is not the leader. The term is the server's: release
// operations run in it, and so do the passes of the tenant operator, whose
// writes are forwarded to the leader like those of an operation (review
// finding CON-6c). The context is cancelled with errReleaseLeadershipEnded
// when the term ends.
func (m *releaseManager) leadershipTerm() context.Context {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.term == nil {
		return nil
	}
	return m.term.ctx
}

// beginOp returns the context of an operation in the current term, with the
// timeout, and the function that ends the operation. It fails on a server
// that is not the leader.
func (m *releaseManager) beginOp(timeout time.Duration) (context.Context, context.CancelFunc, error) {
	m.lock.Lock()
	t := m.term
	if t != nil {
		// Under the lock: a term ended is never added to (waitEndedTerms).
		t.ops.Add(1)
	}
	m.lock.Unlock()
	if t == nil {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusServiceUnavailable,
			"this server is not the leader, and release operations run on the leader; retry the operation")
	}
	ctx, cancel := context.WithTimeout(t.ctx, timeout)
	return ctx, sync.OnceFunc(func() {
		cancel()
		t.ops.Done()
	}), nil
}

// waitEndedTerms waits until the operations of the terms that ended before
// the current one began have stopped, or stopCh closes (false).
func (m *releaseManager) waitEndedTerms(stopCh <-chan struct{}) bool {
	m.lock.Lock()
	var before []*releaseTerm
	if m.term != nil {
		before = m.term.before
	}
	m.lock.Unlock()
	if len(before) == 0 {
		return true
	}
	// The operations were cancelled with their terms; the goroutine ends
	// when the last of them has returned.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for _, t := range before {
			t.ops.Wait()
		}
	}()
	select {
	case <-stopped:
		return true
	case <-stopCh:
		return false
	}
}

// releaseFence is the state an operation put its release in (or found it
// in, for a test run): the release, its phase and its last operation. The
// ID of the last operation tells two operations apart that one user started
// in the same second on two leaders (review finding ATM-17); the other
// fields tell apart those recorded without an ID. An operation sets its ID
// when it records its start, so a release recorded without one is never
// in the state such an operation put it in.
type releaseFence struct {
	id    string
	phase string
	last  *structs.ReleaseOperation
}

func newReleaseFence(rel *structs.Release) *releaseFence {
	f := &releaseFence{id: rel.ID}
	if rel.Status != nil {
		f.phase = rel.Status.Phase
		if op := rel.Status.LastOperation; op != nil {
			last := *op
			f.last = &last
		}
	}
	return f
}

// holds reports whether the release is still in the state of the fence.
func (f *releaseFence) holds(rel *structs.Release) bool {
	if rel == nil || rel.ID != f.id || rel.Status == nil || rel.Status.Phase != f.phase {
		return false
	}
	a, b := rel.Status.LastOperation, f.last
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.Action == b.Action && a.Revision == b.Revision && a.User == b.User &&
		a.StartTime == b.StartTime && a.EndTime == b.EndTime
}

// interruption returns errReleaseInterrupted if a leader change interrupted
// the operation: its term ended, or its release is no longer as it put it.
func (op *releaseOp) interruption() error {
	if op.leadershipEnded() {
		return errReleaseInterrupted
	}
	if op.fence == nil {
		return nil
	}
	snap, err := op.m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	_, rel, err := releaseObject(snap, op.namespace, op.name)
	if err != nil {
		return err
	}
	if !op.fence.holds(rel) {
		return errReleaseInterrupted
	}
	return nil
}

// leadershipEnded reports whether the term of the operation ended.
func (op *releaseOp) leadershipEnded() bool {
	return errors.Is(context.Cause(op.ctx), errReleaseLeadershipEnded)
}

// interrupted reports whether a leader change interrupted the operation.
func (op *releaseOp) interrupted() bool {
	return errors.Is(op.interruption(), errReleaseInterrupted)
}

// interruptedError is the answer of an interrupted operation.
func (op *releaseOp) interruptedError() error {
	op.m.logger.Warn("release operation interrupted by a leader change", "namespace", op.namespace, "release", op.name)
	return structs.NewErrRPCCodedf(http.StatusServiceUnavailable, "release %q: %v", op.name, errReleaseInterrupted)
}

// testInterruptedError is the answer of an interrupted test run.
func (op *releaseOp) testInterruptedError() error {
	op.m.logger.Warn("release test interrupted by a leader change", "namespace", op.namespace, "release", op.name)
	return structs.NewErrRPCCodedf(http.StatusServiceUnavailable, "release %q: %v", op.name, errReleaseTestInterrupted)
}

// answer returns the answer of an operation that failed with err: of an
// interrupted one interruptedError.
func (op *releaseOp) answer(err error) error {
	if errors.Is(err, errReleaseInterrupted) {
		return op.interruptedError()
	}
	return err
}

// writeLatestRelease writes the release over the latest stored version: the
// status of a release changes only while the operation holds its lock, but
// the drift checker may have updated it. It writes only while the release
// is as the operation put it (its fence), and with check-and-set on what it
// read; the fence follows the write.
func (op *releaseOp) writeLatestRelease(rel *structs.Release) (*structs.Release, error) {
	if op.leadershipEnded() {
		return nil, errReleaseInterrupted
	}
	snap, err := op.m.srv.State().Snapshot()
	if err != nil {
		return nil, err
	}
	obj, current, err := releaseObject(snap, rel.Namespace, rel.Name)
	if err != nil {
		return nil, err
	}
	if op.fence != nil && !op.fence.holds(current) {
		return nil, errReleaseInterrupted
	}
	var expected uint64
	if obj != nil {
		expected = obj.ModifyIndex
	}
	written, err := op.m.writeRelease(rel, expected)
	if err == nil && op.fence != nil {
		op.fence = newReleaseFence(written)
	}
	return written, err
}

// deleteRelease deletes the release of an uninstall, while it is as the
// operation put it and with check-and-set on what it read.
func (op *releaseOp) deleteRelease() error {
	if op.leadershipEnded() {
		return errReleaseInterrupted
	}
	snap, err := op.m.srv.State().Snapshot()
	if err != nil {
		return err
	}
	obj, current, err := releaseObject(snap, op.namespace, op.name)
	if err != nil {
		return err
	}
	if op.fence != nil && !op.fence.holds(current) {
		return errReleaseInterrupted
	}
	if obj == nil {
		return nil
	}
	return op.m.deleteObjectAt(structs.ReleaseKind, op.namespace, op.name, obj.ModifyIndex)
}

// writeRevision writes a revision of the release of the operation, with
// check-and-set on expected (0: it must not exist), while the operation is
// not interrupted (review finding ATM-16).
func (op *releaseOp) writeRevision(rev *structs.ReleaseRevision, owner *structs.Release, expected uint64) (*structs.ReleaseRevision, error) {
	op.beforeRevision(rev, false)
	if err := op.interruption(); err != nil {
		return nil, err
	}
	return op.m.writeRevision(rev, owner, expected)
}

// deleteRevision deletes a revision of the release of the operation, while
// it has the modify index it was read with and the operation is not
// interrupted (ATM-16).
func (op *releaseOp) deleteRevision(rev *structs.ReleaseRevision) error {
	op.beforeRevision(rev, true)
	if err := op.interruption(); err != nil {
		return err
	}
	return op.m.deleteObjectAt(structs.ReleaseRevisionKind, op.namespace,
		structs.ReleaseRevisionObjectName(op.name, rev.Revision), rev.ModifyIndex)
}
