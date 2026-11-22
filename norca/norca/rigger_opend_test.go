// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"testing"
	"time"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// An end that can't be written at once is written when it can be: the
// operation neither stays Running nor gets another end than its own.
func TestRiggerOperations_EndWrittenLater(t *testing.T) {
	ci.Parallel(t)

	r := riggerOpTest(t)

	const name = "shop-end-later"

	key := riggerStackKey(structs.DefaultNamespace, "shop")

	run, err := r.s.riggerOps.reserve(key, "shop")
	must.NoError(t, err)

	r.s.riggerOps.mu.Lock()
	run.name = name
	r.s.riggerOps.mu.Unlock()

	op := r.runningOperation(name)

	_, obj, err := r.s.riggerOperation(op.Namespace, name)
	must.NoError(t, err)

	// The writes of an end fail: the kind takes the phase Running alone.
	kind, err := r.s.State().ObjectKindByName(nil, structs.RiggerObjectGroup, structs.RiggerOperationKind)
	must.NoError(t, err)

	broken := *kind
	broken.Schema = strings.Replace(kind.Schema, `"Running", "Succeeded", "Failed", "Cancelled", "Interrupted"`, `"Running"`, 1)
	must.NotEq(t, kind.Schema, broken.Schema)
	must.NoError(t, r.s.State().UpsertObjectKind(structs.MsgTypeTestSetup, r.s.raft.AppliedIndex()+1, &broken))

	w := r.s.newRiggerLogWriter(r.s.riggerLogSubject(name))
	ctx, cancel := t.Context(), func() {}
	done := make(chan struct{})

	go func() {
		defer close(done)

		// Without an engine the operation ends Failed.
		r.s.runRiggerOperation(ctx, cancel, run, func() { r.s.riggerOps.release(key) }, w, nil, nil, nil, op, obj)
	}()

	time.Sleep(1500 * time.Millisecond)

	restored := *kind
	must.NoError(t, r.s.State().UpsertObjectKind(structs.MsgTypeTestSetup, r.s.raft.AppliedIndex()+2, &restored))

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the operation is still ending")
	}

	phase, _ := r.operationPhase(name)
	must.Eq(t, riggerOpFailed, phase)
	must.False(t, r.s.riggerOps.isRunning(op.Namespace, "shop", name))
}
