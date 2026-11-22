// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"testing"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// One operation of a stack runs at a time: a second is refused (409) and
// names the running one, until the first frees its place.
func TestRiggerOperations_Reserve(t *testing.T) {
	ci.Parallel(t)

	var ops riggerOperations

	first, err := ops.reserve("apps/shop", "shop")
	must.NoError(t, err)

	first.name = "shop-1"

	_, err = ops.reserve("apps/shop", "shop")
	must.ErrorContains(t, err, "has a running operation, shop-1")

	other, err := ops.reserve("apps/blog", "blog")
	must.NoError(t, err)
	must.NotNil(t, other)

	must.True(t, ops.isRunning("apps", "shop", "shop-1"))
	must.False(t, ops.isRunning("apps", "shop", "shop-2"))

	ops.release("apps/shop")

	_, err = ops.reserve("apps/shop", "shop")
	must.NoError(t, err)
}

// riggerOpTest writes operations as the leader does, on a test server.
func riggerOpTest(t *testing.T) *registryTest {
	t.Helper()

	return newRegistryTest(t)
}

func (r *registryTest) runningOperation(name string) *RiggerOperation {
	r.t.Helper()

	op := &RiggerOperation{
		Namespace: structs.DefaultNamespace, Name: name,
		Spec:   RiggerOperationSpec{Stack: "shop", Action: riggerOperationApply},
		Status: RiggerOperationStatus{Phase: riggerOpRunning, StartedAt: time.Now().UTC().Format(time.RFC3339)},
	}

	_, err := r.s.writeRiggerOperation(nil, op)
	must.NoError(r.t, err)

	return op
}

func (r *registryTest) operationPhase(name string) (string, *RiggerOperation) {
	op, _, err := r.s.riggerOperation(structs.DefaultNamespace, name)
	if err != nil {
		return "", nil
	}

	return op.Status.Phase, op
}

// A Running operation the leader does not run (its leader changed) is
// marked Interrupted, with what to do; one the leader runs is left alone.
func TestRiggerOperationOperator_Interrupted(t *testing.T) {
	ci.Parallel(t)

	r := riggerOpTest(t)

	// The operation this leader runs.
	run, err := r.s.riggerOps.reserve(riggerStackKey(structs.DefaultNamespace, "blog"), "blog")
	must.NoError(t, err)

	run.name = "blog-1"

	mine := &RiggerOperation{
		Namespace: structs.DefaultNamespace, Name: "blog-1",
		Spec:   RiggerOperationSpec{Stack: "blog", Action: riggerOperationApply},
		Status: RiggerOperationStatus{Phase: riggerOpRunning},
	}
	_, err = r.s.writeRiggerOperation(nil, mine)
	must.NoError(t, err)

	r.runningOperation("shop-1")

	var op *RiggerOperation

	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		var phase string
		phase, op = r.operationPhase("shop-1")

		return phase == riggerOpInterrupted
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the operation of another leader is Interrupted"))

	must.StrContains(t, op.Status.Error, "apply the stack again")
	must.NotEq(t, "", op.Status.FinishedAt)

	phase, _ := r.operationPhase("blog-1")
	must.Eq(t, riggerOpRunning, phase, must.Sprint("the operation this leader runs stays Running"))
}

// A cancel of a running operation stops it through its context and records
// the cancel; a cancel of an ended one is refused; operations are written by
// the servers only.
func TestRigger_CancelOperation(t *testing.T) {
	ci.Parallel(t)

	r := riggerOpTest(t)
	testutil.WaitForLeader(t, r.s.RPC)

	run, err := r.s.riggerOps.reserve(riggerStackKey(structs.DefaultNamespace, "shop"), "shop")
	must.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run.name, run.cancel = "shop-1", cancel
	r.runningOperation("shop-1")

	var resp RiggerOperationResponse
	must.NoError(t, r.s.RPC("Rigger.CancelOperation", &RiggerOperationNameRequest{
		Name: "shop-1", WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: r.token},
	}, &resp))

	must.True(t, resp.Operation.Status.CancelRequested)
	must.ErrorIs(t, ctx.Err(), context.Canceled)
	must.True(t, run.cancelled)

	// Ended: no cancel.
	r.s.finishRiggerOperation(structs.DefaultNamespace, "shop-1", func(st *RiggerOperationStatus) { st.Phase = riggerOpCancelled })

	err = r.s.RPC("Rigger.CancelOperation", &RiggerOperationNameRequest{
		Name: "shop-1", WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: r.token},
	}, &resp)
	must.ErrorContains(t, err, "has ended: Cancelled")

	// The object API writes no operation.
	var wresp structs.CustomObjectWriteResponse
	err = r.s.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
		Object: &structs.CustomObject{
			Group: structs.RiggerObjectGroup, Kind: structs.RiggerOperationKind, Name: "forged",
			Spec: map[string]any{"stack": "shop", "action": "apply"},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: r.token},
	}, &wresp)
	must.ErrorContains(t, err, "written by the servers")
}
