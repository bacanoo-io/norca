// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// An apply with a token other than a management token leaves the finalizer
// of the servers (norca.io/operators) out of the finalizers it adds (review
// finding SEC-19, the part the finalizers endpoint did not cover): on an
// object no built-in operator works on, it would hold the deletion back for
// good. It is left out, not refused, so that an exported object and a
// selective backup restore, which applies the object as it was, still go
// through; a built-in operator puts it on its own objects itself. Its other
// finalizers are added; a management token may add it.
func TestCustomObjectEndpoint_ApplyLeavesOutOperatorsFinalizer(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	team := mock.CreatePolicyAndToken(t, s.State(), 1001, "team", `
namespace "default" {
  object "db.example.com/*" { capabilities = ["read", "write", "manage-finalizers"] }
}`)
	_, err := c.upsertKind(testPGKind(true), false, root.SecretID)
	must.NoError(t, err)

	const own = "example.com/cleanup"
	obj := testPGObject("default", "billing", map[string]any{"version": "16"})
	obj.Finalizers = []string{structs.OperatorsFinalizer, own}
	resp, err := c.apply(obj, nil, false, team.SecretID)
	must.NoError(t, err)
	must.Eq(t, []string{own}, resp.Object.Finalizers)
	stored, err := c.get("default", "billing", root.SecretID)
	must.NoError(t, err)
	must.Eq(t, []string{own}, stored.Finalizers)

	// The object can be deleted once its own finalizer is gone.
	_, err = c.finalizers("default", "billing", nil, []string{own}, team.SecretID)
	must.NoError(t, err)

	// A management token adds it as given.
	other := testPGObject("default", "kept", map[string]any{"version": "16"})
	other.Finalizers = []string{structs.OperatorsFinalizer}
	resp, err = c.apply(other, nil, false, root.SecretID)
	must.NoError(t, err)
	must.Eq(t, []string{structs.OperatorsFinalizer}, resp.Object.Finalizers)

	// An apply of the object as it is keeps the finalizer it has.
	again := testPGObject("default", "kept", map[string]any{"version": "17"})
	again.Finalizers = []string{structs.OperatorsFinalizer}
	resp, err = c.apply(again, nil, false, team.SecretID)
	must.NoError(t, err)
	must.Eq(t, []string{structs.OperatorsFinalizer}, resp.Object.Finalizers)
}
