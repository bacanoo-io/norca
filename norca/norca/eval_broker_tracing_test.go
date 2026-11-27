// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// testTraceParent returns a valid sampled traceparent that differs by n.
func testTraceParent(n int) string {
	return fmt.Sprintf("00-%032x-%016x-01", n+1, n+1)
}

// tracedEvals returns the number of trace parents the broker keeps.
func tracedEvals(b *EvalBroker) int {
	b.l.RLock()
	defer b.l.RUnlock()
	return len(b.traceParents)
}

// The trace parent of an evaluation is returned for its outstanding dequeue
// only, and removed when the evaluation is acked or nacked: a redelivered
// evaluation starts a new trace.
func TestEvalBroker_TraceParent_AckNack(t *testing.T) {
	ci.Parallel(t)
	b := testBroker(t, 0)
	b.SetEnabled(true)

	acked, nacked := mock.Eval(), mock.Eval()
	b.EnqueueTraced(acked, testTraceParent(1))
	b.EnqueueTraced(nacked, testTraceParent(2))
	must.Eq(t, 2, tracedEvals(b))

	// enqueued again: the first parent stays
	b.EnqueueTraced(acked, testTraceParent(3))
	must.Eq(t, 2, tracedEvals(b))

	for range 2 {
		eval, token, err := b.Dequeue(defaultSched, time.Second)
		must.NoError(t, err)
		must.NotNil(t, eval)
		must.Eq(t, "", b.TraceParent(eval.ID, "wrong-token"))
		switch eval.ID {
		case acked.ID:
			must.Eq(t, testTraceParent(1), b.TraceParent(eval.ID, token))
			must.NoError(t, b.Ack(eval.ID, token))
		case nacked.ID:
			must.Eq(t, testTraceParent(2), b.TraceParent(eval.ID, token))
			must.NoError(t, b.Nack(eval.ID, token))
		}
	}
	must.Eq(t, 0, tracedEvals(b))

	// the nacked evaluation is delivered again without a parent
	eval, token, err := b.Dequeue(defaultSched, time.Second)
	must.NoError(t, err)
	must.Eq(t, nacked.ID, eval.ID)
	must.Eq(t, "", b.TraceParent(eval.ID, token))
	must.NoError(t, b.Ack(eval.ID, token))
	must.Eq(t, 0, tracedEvals(b))
}

// An evaluation whose nack timer expires loses its trace parent.
func TestEvalBroker_TraceParent_NackTimeout(t *testing.T) {
	ci.Parallel(t)
	b := testBroker(t, 20*time.Millisecond)
	b.SetEnabled(true)

	eval := mock.Eval()
	b.EnqueueTraced(eval, testTraceParent(1))
	out, _, err := b.Dequeue(defaultSched, time.Second)
	must.NoError(t, err)
	must.Eq(t, eval.ID, out.ID)

	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return tracedEvals(b) == 0 }),
		wait.Timeout(5*time.Second),
		wait.Gap(10*time.Millisecond),
	))
}

// Pending evaluations of a job that the broker marks cancelable, and a
// flush when the leader steps down, drop their trace parents.
func TestEvalBroker_TraceParent_CancelAndFlush(t *testing.T) {
	ci.Parallel(t)
	b := testBroker(t, 0)
	b.SetEnabled(true)

	job := mock.Job()
	evals := make([]*structs.Evaluation, 3)
	for i := range evals {
		e := mock.Eval()
		e.JobID = job.ID
		e.ModifyIndex = uint64(10 + i)
		evals[i] = e
		b.EnqueueTraced(e, testTraceParent(i))
	}
	must.Eq(t, 3, tracedEvals(b))

	// the first is ready, the others pending; its ack keeps the newest
	// pending one and cancels the rest
	eval, token, err := b.Dequeue(defaultSched, time.Second)
	must.NoError(t, err)
	must.NoError(t, b.Ack(eval.ID, token))
	must.Eq(t, 1, tracedEvals(b))
	must.Eq(t, 1, b.Stats().TotalCancelable)

	b.EnqueueTraced(mock.Eval(), testTraceParent(9))
	must.Eq(t, 2, tracedEvals(b))
	b.SetEnabled(false)
	must.Eq(t, 0, tracedEvals(b))

	// a disabled broker keeps nothing
	b.EnqueueTraced(mock.Eval(), testTraceParent(10))
	must.Eq(t, 0, tracedEvals(b))
}

// The broker keeps at most maxTracedEvals trace parents; evaluations
// beyond it are enqueued untraced.
func TestEvalBroker_TraceParent_Bounded(t *testing.T) {
	ci.Parallel(t)
	b := testBroker(t, 0)
	b.SetEnabled(true)

	for i := range maxTracedEvals + 10 {
		b.EnqueueTraced(mock.Eval(), testTraceParent(i))
	}
	must.Eq(t, maxTracedEvals, tracedEvals(b))
	must.Eq(t, maxTracedEvals+10, b.Stats().TotalReady)
}
