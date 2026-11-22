// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Without a log, as on servers without a bus, an operation is followed by
// its phase.
func TestRiggerOperations_FollowWithoutLog(t *testing.T) {
	ci.Parallel(t)

	r := riggerOpTest(t)

	const name = "shop-unlogged"

	key := riggerStackKey(structs.DefaultNamespace, "shop")

	run, err := r.s.riggerOps.reserve(key, "shop")
	must.NoError(t, err)

	r.s.riggerOps.mu.Lock()
	run.name = name
	r.s.riggerOps.mu.Unlock()

	op := r.runningOperation(name)

	read := func() *RiggerOperationLogResponse {
		var out RiggerOperationLogResponse

		must.NoError(t, r.s.RPC("Rigger.OperationLog", &RiggerOperationLogRequest{
			Name: name, Wait: 2 * time.Second,
			QueryOptions: structs.QueryOptions{Region: r.s.Region(), Namespace: op.Namespace, AuthToken: r.token},
		}, &out))

		return &out
	}

	start := time.Now()
	page := read()

	must.Eq(t, riggerOpRunning, page.Phase)
	must.False(t, page.Finished)
	must.NotEq(t, "", page.LogUnavailable)
	must.GreaterEq(t, 1500*time.Millisecond, time.Since(start), must.Sprint("a read waits, or a follower spins"))

	r.s.finishRiggerOperation(op.Namespace, name, func(st *RiggerOperationStatus) { st.Phase = riggerOpSucceeded })
	r.s.riggerOps.release(key)

	page = read()

	must.Eq(t, riggerOpSucceeded, page.Phase)
	must.True(t, page.Finished)
}
